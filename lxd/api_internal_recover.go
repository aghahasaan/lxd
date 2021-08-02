package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/osarch"
)

// Define API endpoints for recover actions.
var internalRecoverValidateCmd = APIEndpoint{
	Path: "recover/validate",

	Post: APIEndpointAction{Handler: internalRecoverValidate},
}

var internalRecoverImportCmd = APIEndpoint{
	Path: "recover/import",

	Post: APIEndpointAction{Handler: internalRecoverImport},
}

// init recover adds API endpoints to handler slice.
func init() {
	apiInternal = append(apiInternal, internalRecoverValidateCmd, internalRecoverImportCmd)
}

// internalRecoverValidatePost is used to initiate a recovery validation scan.
type internalRecoverValidatePost struct {
	Pools []api.StoragePoolsPost `json:"pools" yaml:"pools"`
}

// internalRecoverValidateVolume provides info about a missing volume that the recovery validation scan found.
type internalRecoverValidateVolume struct {
	Name          string `json:"name" yaml:"name"`                   // Name of volume.
	Type          string `json:"type" yaml:"type"`                   // Same as Type from StorageVolumesPost (container, custom or virtual-machine).
	SnapshotCount int    `json:"snapshotCount" yaml:"snapshotCount"` // Count of snapshots found for volume.
	Project       string `json:"project" yaml:"project"`             // Project the volume belongs to.
	Pool          string `json:"pool" yaml:"pool"`                   // Pool the volume belongs to.
}

// internalRecoverValidateResult returns the result of the validation scan.
type internalRecoverValidateResult struct {
	UnknownVolumes   []internalRecoverValidateVolume // Volumes that could be imported.
	DependencyErrors []string                        // Errors that are preventing import from proceeding.
}

// internalRecoverImportPost is used to initiate a recovert import.
type internalRecoverImportPost struct {
	Pools []api.StoragePoolsPost `json:"pools" yaml:"pools"`
}

// internalRecoverScan provides the discovery and import functionality for both recovery validate and import steps.
func internalRecoverScan(d *Daemon, userPools []api.StoragePoolsPost, validateOnly bool) response.Response {
	var err error
	var projects map[string]*db.Project
	var projectProfiles map[string][]*api.Profile
	var networks map[int64]api.Network

	// Retrieve all project, profile and network info in a single transaction so we can use it for all
	// imported instances and volumes, and avoid repeatedly querying the same information.
	err = d.State().Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Load list of projects for validation.
		ps, err := tx.GetProjects(db.ProjectFilter{})
		if err != nil {
			return err
		}

		// Convert to map for lookups by name later.
		projects = make(map[string]*db.Project, len(ps))
		for i := range ps {
			projects[ps[i].Name] = &ps[i]
		}

		// Load list of project/profile names for validation.
		profiles, err := tx.GetProfiles(db.ProfileFilter{})
		if err != nil {
			return err
		}

		// Convert to map for lookups by project name later.
		projectProfiles = make(map[string][]*api.Profile)
		for _, profile := range profiles {
			if projectProfiles[profile.Project] == nil {
				projectProfiles[profile.Project] = []*api.Profile{db.ProfileToAPI(&profile)}
			} else {
				projectProfiles[profile.Project] = append(projectProfiles[profile.Project], db.ProfileToAPI(&profile))
			}
		}

		// Load list of project/network names for validation.
		networks, err = tx.GetCreatedNetworks()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Failed getting validate dependency check info"))
	}

	res := internalRecoverValidateResult{}

	revert := revert.New()
	defer revert.Fail()

	// addDependencyError adds an error to the list of dependency errors if not already present in list.
	addDependencyError := func(err error) {
		errStr := err.Error()

		if !shared.StringInSlice(errStr, res.DependencyErrors) {
			res.DependencyErrors = append(res.DependencyErrors, errStr)
		}
	}

	// Used to store the unknown volumes for each pool & project.
	poolsProjectVols := make(map[string]map[string][]*backup.Config)

	// Used to store a handle to each pool containing user supplied config.
	pools := make(map[string]storagePools.Pool)

	// Iterate the pools finding unknown volumes and perform validation.
	for _, p := range userPools {
		pool, err := storagePools.GetPoolByName(d.State(), p.Name)
		if err != nil {
			if errors.Cause(err) == db.ErrNoSuchObject {
				// If pool doesn't exist in DB, initialise a temporary pool with the supplied info.
				poolInfo := api.StoragePool{
					Name:           p.Name,
					Driver:         p.Driver,
					StoragePoolPut: p.StoragePoolPut,
					Status:         api.StoragePoolStatusCreated,
				}

				pool, err = storagePools.NewTemporary(d.State(), &poolInfo)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed to initialise unknown pool %q", p.Name))
				}

				err = pool.Driver().Validate(poolInfo.Config)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed config validation for unknown pool %q", p.Name))
				}
			} else {
				return response.SmartError(errors.Wrapf(err, "Failed loading existing pool %q", p.Name))
			}
		}

		// Record this pool to be used during import stage, assuming validation passes.
		pools[p.Name] = pool

		// Try to mount the pool.
		ourMount, err := pool.Mount()
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed mounting pool %q", pool.Name()))
		}

		// Unmount pool when done if not existing in DB after function has finished.
		// This way if we are dealing with an existing pool or have successfully created the DB record then
		// we won't unmount it. As we should leave successfully imported pools mounted.
		if ourMount {
			defer func() {
				cleanupPool := pools[pool.Name()]
				if cleanupPool != nil && cleanupPool.ID() == storagePools.PoolIDTemporary {
					cleanupPool.Unmount()
				}
			}()

			revert.Add(func() {
				cleanupPool := pools[pool.Name()]
				cleanupPool.Unmount() // Defer won't do it if record exists, so unmount on failure.
			})
		}

		// Get list of unknown volumes on pool.
		poolProjectVols, err := pool.ListUnknownVolumes(nil)
		if err != nil {
			if errors.Cause(err) == storageDrivers.ErrNotImplemented {
				logger.Error("Pool driver hasn't implemented recovery yet, skipping", log.Ctx{"pool": pool.Name(), "err": err})
			} else {
				return response.SmartError(errors.Wrapf(err, "Failed validating volumes on pool %q", pool.Name()))
			}
		}

		// Store for consumption after validation scan to avoid needing to reprocess.
		poolsProjectVols[p.Name] = poolProjectVols

		// Check dependencies are met for each volume.
		for projectName, poolVols := range poolProjectVols {
			// Check project exists in database.
			projectInfo := projects[projectName]

			// Look up effective project names for profiles and networks.
			var profileProjectname string
			if projectInfo != nil {
				profileProjectname = project.ProfileProjectFromRecord(projectInfo)
			} else {
				addDependencyError(fmt.Errorf("Project %q", projectName))
				continue // Skip further validation if project is missing.
			}

			for _, poolVol := range poolVols {
				if poolVol.Container == nil {
					continue // Skip non-instance volumes.
				}

				// Check that the instance's profile dependencies are met.
				for _, poolInstProfileName := range poolVol.Container.Profiles {
					foundProfile := false
					for _, profile := range projectProfiles[profileProjectname] {
						if profile.Name == poolInstProfileName {
							foundProfile = true
						}
					}

					if !foundProfile {
						addDependencyError(fmt.Errorf("Profile %q in project %q", poolInstProfileName, projectName))
					}
				}

				// Check that the instance's NIC network dependencies are met.
				for _, devConfig := range poolVol.Container.ExpandedDevices {
					if devConfig["type"] != "nic" {
						continue
					}

					if devConfig["network"] == "" {
						continue
					}

					foundNetwork := false
					for _, n := range networks {
						if n.Name == devConfig["network"] {
							foundNetwork = true
							break
						}
					}

					if !foundNetwork {
						addDependencyError(fmt.Errorf("Network %q in project %q", devConfig["network"], projectName))
					}
				}
			}
		}
	}

	// If in validation mode or if there are dependency errors, return discovered unknown volumes, along with
	// any dependency errors.
	if validateOnly || len(res.DependencyErrors) > 0 {
		for poolName, poolProjectVols := range poolsProjectVols {
			for projectName, poolVols := range poolProjectVols {
				for _, poolVol := range poolVols {
					if poolVol.Container == nil {
						continue // Skip non-instance volumes.
					}

					res.UnknownVolumes = append(res.UnknownVolumes, internalRecoverValidateVolume{
						Type:          poolVol.Container.Type,
						Name:          poolVol.Container.Name,
						Pool:          poolName,
						Project:       projectName,
						SnapshotCount: len(poolVol.Snapshots),
					})
				}
			}
		}

		return response.SyncResponse(true, &res)
	}

	// If in import mode and no dependency errors, then re-create missing DB records.

	// Create any missing instance and storage volume records.
	for _, pool := range pools {
		for projectName, poolVols := range poolsProjectVols[pool.Name()] {
			projectInfo := projects[projectName]

			if projectInfo == nil {
				// Shouldn't happen as we validated this above, but be sure for safety.
				return response.SmartError(fmt.Errorf("Project %q not found", projectName))
			}

			profileProjectname := project.ProfileProjectFromRecord(projectInfo)

			for _, poolVol := range poolVols {
				if poolVol.Container == nil {
					continue // Skip non-instance volumes.
				}

				// Create missing storage pool DB record if neeed.
				if pool.ID() == storagePools.PoolIDTemporary {
					if poolVol.Pool != nil {
						// Create storage pool DB record from config in the instance.
						logger.Info("Creating storage pool DB record from instance config", log.Ctx{"name": poolVol.Pool.Name, "description": poolVol.Pool.Description, "driver": poolVol.Pool.Driver, "config": poolVol.Pool.Config})
						_, err = dbStoragePoolCreateAndUpdateCache(d.State(), poolVol.Pool.Name, poolVol.Pool.Description, poolVol.Pool.Driver, poolVol.Pool.Config)
						if err != nil {
							return response.SmartError(errors.Wrapf(err, "Failed creating storage pool %q database entry", pool.Name()))
						}
					} else {
						// Create storage pool DB record from config supplied by user.
						poolDriverName := pool.Driver().Info().Name
						poolDriverConfig := pool.Driver().Config()
						logger.Info("Creating storage pool DB record from user config", log.Ctx{"name": pool.Name(), "driver": poolDriverName, "config": poolDriverConfig})
						_, err = dbStoragePoolCreateAndUpdateCache(d.State(), pool.Name(), "", poolDriverName, poolDriverConfig)
						if err != nil {
							return response.SmartError(errors.Wrapf(err, "Failed creating storage pool %q database entry", pool.Name()))
						}
					}

					revert.Add(func() {
						dbStoragePoolDeleteAndUpdateCache(d.State(), pool.Name())
					})

					logger.Debug("Marked storage pool local status as created", log.Ctx{"pool": pool.Name()})

					newPool, err := storagePools.GetPoolByName(d.State(), pool.Name())
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed loading created storage pool %q", pool.Name()))
					}

					// Record this newly created pool so that defer doesn't unmount on return.
					pools[pool.Name()] = newPool
					pool = newPool // Replace temporary pool handle with proper one from DB.
				}

				// Recover instance.
				profiles := make([]api.Profile, 0, len(poolVol.Container.Profiles))
				for _, profileName := range poolVol.Container.Profiles {
					for i := range projectProfiles[profileProjectname] {
						if projectProfiles[profileProjectname][i].Name == profileName {
							profiles = append(profiles, *projectProfiles[profileProjectname][i])
						}
					}
				}

				inst, err := internalRecoverImportInstance(d.State(), pool, projectName, poolVol, profiles, revert)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed importing instance %q in project %q", poolVol.Container.Name, projectName))
				}

				// Recover instance snapshots.
				for _, poolInstSnap := range poolVol.Snapshots {
					profiles := make([]api.Profile, 0, len(poolInstSnap.Profiles))
					for _, profileName := range poolInstSnap.Profiles {
						for i := range projectProfiles[profileProjectname] {
							if projectProfiles[profileProjectname][i].Name == profileName {
								profiles = append(profiles, *projectProfiles[profileProjectname][i])
							}
						}
					}

					err = internalRecoverImportInstanceSnapshot(d.State(), pool, projectName, poolVol, poolInstSnap, profiles, revert)
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed importing instance %q snapshot %q in project %q", poolVol.Container.Name, poolInstSnap.Name, projectName))
					}
				}

				// Recreate instance mount path and symlinks (must come after snapshot recovery).
				err = pool.ImportInstance(inst, nil)
				if err != nil {
					return response.SmartError(errors.Wrap(err, "Failed importing instance"))
				}

				// Reinitialise the instance's root disk quota even if no size specified (allows the storage driver the
				// opportunity to reinitialise the quota based on the new storage volume's DB ID).
				_, rootConfig, err := shared.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
				if err == nil {
					err = pool.SetInstanceQuota(inst, rootConfig["size"], rootConfig["size.state"], nil)
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed reinitializing root disk quota %q", rootConfig["size"]))
					}
				}
			}
		}
	}

	revert.Success()
	return response.EmptySyncResponse
}

// internalRecoverImportInstance recreates the database records for an instance and returns the new instance.
func internalRecoverImportInstance(s *state.State, pool storagePools.Pool, projectName string, poolVol *backup.Config, profiles []api.Profile, revert *revert.Reverter) (instance.Instance, error) {
	if poolVol.Container == nil {
		return nil, fmt.Errorf("Pool volume is not an instance volume")
	}

	// Add root device if needed.
	if poolVol.Container.Devices == nil {
		poolVol.Container.Devices = make(map[string]map[string]string, 0)
	}

	if poolVol.Container.ExpandedDevices == nil {
		poolVol.Container.ExpandedDevices = make(map[string]map[string]string, 0)
	}

	internalImportRootDevicePopulate(pool.Name(), poolVol.Container.Devices, poolVol.Container.ExpandedDevices, profiles)

	arch, err := osarch.ArchitectureId(poolVol.Container.Architecture)
	if err != nil {
		return nil, err
	}

	instanceType, err := instancetype.New(poolVol.Container.Type)
	if err != nil {
		return nil, err
	}

	// Extract volume config from backup file if present.
	var volConfig map[string]string
	if poolVol.Volume != nil {
		volConfig = poolVol.Volume.Config
	}

	inst, err := instance.CreateInternal(s, db.InstanceArgs{
		Project:      projectName,
		Architecture: arch,
		BaseImage:    poolVol.Container.Config["volatile.base_image"],
		Config:       poolVol.Container.Config,
		CreationDate: poolVol.Container.CreatedAt,
		Type:         instanceType,
		Description:  poolVol.Container.Description,
		Devices:      deviceConfig.NewDevices(poolVol.Container.Devices),
		Ephemeral:    poolVol.Container.Ephemeral,
		LastUsedDate: poolVol.Container.LastUsedAt,
		Name:         poolVol.Container.Name,
		Profiles:     poolVol.Container.Profiles,
		Stateful:     poolVol.Container.Stateful,
	}, false, volConfig, revert)
	if err != nil {
		return nil, errors.Wrap(err, "Failed creating instance record")
	}

	return inst, err
}

// internalRecoverImportInstance recreates the database records for an instance snapshot.
func internalRecoverImportInstanceSnapshot(s *state.State, pool storagePools.Pool, projectName string, poolVol *backup.Config, snap *api.InstanceSnapshot, profiles []api.Profile, revert *revert.Reverter) error {
	if poolVol.Container == nil || snap == nil {
		return fmt.Errorf("Pool volume is not an instance volume")
	}

	// Add root device if needed.
	if snap.Devices == nil {
		snap.Devices = make(map[string]map[string]string, 0)
	}

	if snap.ExpandedDevices == nil {
		snap.ExpandedDevices = make(map[string]map[string]string, 0)
	}

	internalImportRootDevicePopulate(pool.Name(), snap.Devices, snap.ExpandedDevices, profiles)

	arch, err := osarch.ArchitectureId(snap.Architecture)
	if err != nil {
		return err
	}

	instanceType, err := instancetype.New(poolVol.Container.Type)
	if err != nil {
		return err
	}

	_, err = instance.CreateInternal(s, db.InstanceArgs{
		Project:      projectName,
		Architecture: arch,
		BaseImage:    snap.Config["volatile.base_image"],
		Config:       snap.Config,
		CreationDate: snap.CreatedAt,
		Type:         instanceType,
		Snapshot:     true,
		Devices:      deviceConfig.NewDevices(snap.Devices),
		Ephemeral:    snap.Ephemeral,
		LastUsedDate: snap.LastUsedAt,
		Name:         poolVol.Container.Name + shared.SnapshotDelimiter + snap.Name,
		Profiles:     snap.Profiles,
		Stateful:     snap.Stateful,
	}, false, nil, revert)
	if err != nil {
		return errors.Wrapf(err, "Failed creating instance snapshot record %q", snap.Name)
	}

	return nil
}

// internalRecoverValidate validates the requested pools to be recovered.
func internalRecoverValidate(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	req := &internalRecoverValidatePost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	return internalRecoverScan(d, req.Pools, true)
}

// internalRecoverImport performs the pool volume recovery.
func internalRecoverImport(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	req := &internalRecoverImportPost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	return internalRecoverScan(d, req.Pools, false)
}
