package volumes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/common"
	"github.com/milosgajdos83/docker/plugins"
)

type Repository struct {
	pluginRepository *plugins.Repository
	configPath       string
	driver           graphdriver.Driver
	volumes          map[string]*Volume
	lock             sync.Mutex
}

func NewRepository(pluginReposistory *plugins.Repository, configPath string, driver graphdriver.Driver) (*Repository, error) {
	abspath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}

	// Create the config path
	if err := os.MkdirAll(abspath, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	repo := &Repository{
		driver:     driver,
		configPath: abspath,
		volumes:    make(map[string]*Volume),
	}

	return repo, repo.restore()
}

func (r *Repository) newVolume(path string, writable bool) (*Volume, error) {
	var (
		isBindMount bool
		err         error
		id          = common.GenerateRandomID()
	)
	if path != "" {
		isBindMount = true
	}

	if path == "" {
		path, err = r.createNewVolumePath(id)
		if err != nil {
			return nil, err
		}
	}
	path = filepath.Clean(path)

	// Ignore the error here since the path may not exist
	// Really just want to make sure the path we are using is real(or non-existant)
	if cleanPath, err := filepath.EvalSymlinks(path); err == nil {
		path = cleanPath
	}

	v := &Volume{
		ID:          id,
		Path:        path,
		repository:  r,
		Writable:    writable,
		containers:  make(map[string]struct{}),
		configPath:  r.configPath + "/" + id,
		IsBindMount: isBindMount,
	}

	if err := v.initialize(); err != nil {
		return nil, err
	}

	r.add(v)
	return v, nil
}

func (r *Repository) restore() error {
	dir, err := ioutil.ReadDir(r.configPath)
	if err != nil {
		return err
	}

	for _, v := range dir {
		id := v.Name()
		vol := &Volume{
			ID:         id,
			configPath: r.configPath + "/" + id,
			containers: make(map[string]struct{}),
		}
		if err := vol.FromDisk(); err != nil {
			if !os.IsNotExist(err) {
				log.Debugf("Error restoring volume: %v", err)
				continue
			}
			if err := vol.initialize(); err != nil {
				log.Debugf("%s", err)
				continue
			}
		}
		r.add(vol)
	}
	return nil
}

func (r *Repository) Get(path string) *Volume {
	r.lock.Lock()
	vol := r.get(path)
	r.lock.Unlock()
	return vol
}

func (r *Repository) get(path string) *Volume {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	return r.volumes[filepath.Clean(path)]
}

func (r *Repository) add(volume *Volume) {
	if vol := r.get(volume.Path); vol != nil {
		log.Debugf("Volume exists: %s", volume.ID)
		return
	}
	r.volumes[volume.Path] = volume
	return
}

func (r *Repository) Delete(path string) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	volume := r.get(filepath.Clean(path))
	if volume == nil {
		return fmt.Errorf("Volume %s does not exist", path)
	}

	containers := volume.Containers()
	if len(containers) > 0 {
		return fmt.Errorf("Volume %s is being used and cannot be removed: used by containers %s", volume.Path, containers)
	}

	if err := os.RemoveAll(volume.configPath); err != nil {
		return err
	}

	if !volume.IsBindMount {
		if err := r.driver.Remove(volume.ID); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
	}

	delete(r.volumes, volume.Path)
	return nil
}

func (r *Repository) createNewVolumePath(id string) (string, error) {
	if err := r.driver.Create(id, ""); err != nil {
		return "", err
	}

	path, err := r.driver.Get(id, "")
	if err != nil {
		return "", fmt.Errorf("Driver %s failed to get volume rootfs %s: %v", r.driver, id, err)
	}

	return path, nil
}

type VolumeExtensionReq struct {
	DockerVolumesExtensionVersion int
	HostPath                      string
	ContainerID                   string
}

type VolumeExtensionResp struct {
	ModifiedHostPath              string
	DockerVolumesExtensionVersion int
}

func (r *Repository) FindOrCreateVolume(path, containerId string, writable bool) (*Volume, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	plugins := r.pluginRepository.GetPlugins("volume")

	for _, plugin := range plugins {
		data := VolumeExtensionReq{
			HostPath:    path,
			ContainerID: containerId,
		}

		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}

		log.Debugf("sending request for volume extension:\n%s", string(b))
		resp, err := plugin.Call("POST", "volumes", bytes.NewBuffer(b))
		if err != nil {
			return nil, fmt.Errorf("got error calling volume extension: %v", err)
		}
		defer resp.Body.Close()

		var extResp VolumeExtensionResp
		log.Debugf("decoding volume extension response")
		if err := json.NewDecoder(resp.Body).Decode(&extResp); err != nil {
			return nil, err
		}

		// Use the path provided by the extension instead of creating one
		if extResp.ModifiedHostPath != "" {
			log.Debugf("using modified host path for volume extension")
			path = extResp.ModifiedHostPath
		}
	}

	if path == "" {
		return r.newVolume(path, writable)
	}

	if v := r.get(path); v != nil {
		return v, nil
	}

	return r.newVolume(path, writable)
}
