package daemon

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/volume"
)

type MountPoint struct {
	Name        string
	Destination string
	Driver      string
	RW          bool
	Volume      volume.Volume `json:"-"`
	source      string
}

func (m *MountPoint) Setup() (string, error) {
	if m.Volume != nil {
		return m.Volume.Mount()
	}

	if len(m.source) > 0 {
		if _, err := os.Stat(m.source); err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			if err := os.MkdirAll(m.source, 0755); err != nil {
				return "", err
			}
		}
		return m.source, nil
	}

	return "", fmt.Errorf("Unable to setup mount point, neither source nor volume defined")
}

func (m *MountPoint) Source() string {
	if m.Volume != nil {
		return m.Volume.Path()
	}

	return m.source
}

func parseBindMount(container *Container, spec string) (*MountPoint, error) {
	bind := &MountPoint{
		RW: true,
	}
	arr := strings.Split(spec, ":")

	switch len(arr) {
	case 2:
		bind.Destination = arr[1]
	case 3:
		bind.Destination = arr[1]
		bind.RW = validMountMode(arr[2]) && arr[2] == "rw"
	default:
		return nil, fmt.Errorf("Invalid volume specification: %s", spec)
	}

	if !filepath.IsAbs(arr[0]) {
		bind.Name = arr[0]
		bind.Driver = container.Config.VolumeDriver
	} else {
		bind.source = filepath.Clean(arr[0])
	}

	bind.Destination = filepath.Clean(bind.Destination)
	return bind, nil
}

func parseVolumesFrom(spec string) (string, string, error) {
	specParts := strings.SplitN(spec, ":", 2)
	if len(specParts) == 0 {
		return "", "", fmt.Errorf("malformed volumes-from specification: %s", spec)
	}
	var (
		id   = specParts[0]
		mode = "rw"
	)
	if len(specParts) == 2 {
		mode = specParts[1]
		if !validMountMode(mode) {
			return "", "", fmt.Errorf("invalid mode for volumes-from: %s", mode)
		}
	}
	return id, mode, nil
}

func validMountMode(mode string) bool {
	validModes := map[string]bool{
		"rw": true,
		"ro": true,
	}
	return validModes[mode]
}

func copyExistingContents(source, destination string) error {
	volList, err := ioutil.ReadDir(source)
	if err != nil {
		return err
	}
	if len(volList) > 0 {
		srcList, err := ioutil.ReadDir(destination)
		if err != nil {
			return err
		}
		if len(srcList) == 0 {
			// If the source volume is empty copy files from the root into the volume
			if err := chrootarchive.CopyWithTar(source, destination); err != nil {
				return err
			}
		}
	}
	return copyOwnership(source, destination)
}

func (daemon *Daemon) registerMountPoints(container *Container, hostConfig *runconfig.HostConfig) error {
	binds := map[string]bool{}
	mountPoints := map[string]*MountPoint{}

	for name, point := range container.MountPoints {
		mountPoints[name] = point
	}

	for _, v := range hostConfig.VolumesFrom {
		containerID, mode, err := parseVolumesFrom(v)
		if err != nil {
			return err
		}

		c, err := daemon.Get(containerID)
		if err != nil {
			return err
		}

		for _, m := range c.MountPoints {
			v, err := daemon.createVolume(m.Name, m.Driver)
			if err != nil {
				return err
			}

			cp := m
			cp.RW = mode != "ro"
			cp.Volume = v

			mountPoints[cp.Destination] = cp
		}
	}

	for _, b := range hostConfig.Binds {
		// #10618
		bind, err := parseBindMount(container, b)
		if err != nil {
			return err
		}

		if binds[bind.Destination] {
			return fmt.Errorf("Duplicate bind mount %s", bind.Destination)
		}

		if len(bind.Name) > 0 && len(bind.Driver) > 0 {
			v, err := daemon.createVolume(bind.Name, bind.Driver)
			if err != nil {
				return err
			}
			bind.Volume = v
		}

		binds[bind.Destination] = true
		mountPoints[bind.Destination] = bind
	}

	container.MountPoints = mountPoints

	return nil
}

func (daemon *Daemon) verifyOldVolumesInfo(container *Container) error {
	jsonPath, err := container.jsonPath()
	if err != nil {
		return err
	}
	f, err := os.Open(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type oldContVolCfg struct {
		Volumes   map[string]string
		VolumesRW map[string]bool
	}

	var vols oldContVolCfg
	if err := json.NewDecoder(f).Decode(&vols); err != nil {
		return err
	}

	for destination, hostPath := range vols.Volumes {
		vfsPath := filepath.Join(daemon.root, "vfs", "dir")

		if strings.HasPrefix(hostPath, vfsPath) {
			id := filepath.Base(hostPath)

			container.MountPoints[destination] = &MountPoint{
				Name:        id,
				Driver:      "local",
				Destination: destination,
				RW:          vols.VolumesRW[destination],
			}
		}
	}

	return container.ToDisk()
}
