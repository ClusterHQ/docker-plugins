package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/graph"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/runconfig"
	"github.com/docker/libcontainer/label"
)

func (daemon *Daemon) ContainerCreate(name string, config *runconfig.Config, hostConfig *runconfig.HostConfig) (string, []string, error) {
	warnings, err := daemon.verifyHostConfig(hostConfig)
	if err != nil {
		return "", warnings, err
	}

	// The check for a valid workdir path is made on the server rather than in the
	// client. This is because we don't know the type of path (Linux or Windows)
	// to validate on the client.
	if config.WorkingDir != "" && !filepath.IsAbs(config.WorkingDir) {
		return "", warnings, fmt.Errorf("The working directory '%s' is invalid. It needs to be an absolute path.", config.WorkingDir)
	}

	if !daemon.SystemConfig().OomKillDisable {
		hostConfig.OomKillDisable = false
		return "", warnings, fmt.Errorf("Your kernel does not support oom kill disable.")
	}

	container, buildWarnings, err := daemon.Create(config, hostConfig, name)
	if err != nil {
		if daemon.Graph().IsNotExist(err, config.Image) {
			_, tag := parsers.ParseRepositoryTag(config.Image)
			if tag == "" {
				tag = graph.DEFAULTTAG
			}
			return "", warnings, fmt.Errorf("No such image: %s (tag: %s)", config.Image, tag)
		}
		return "", warnings, err
	}

	container.LogEvent("create")
	warnings = append(warnings, buildWarnings...)

	return container.ID, warnings, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) Create(config *runconfig.Config, hostConfig *runconfig.HostConfig, name string) (*Container, []string, error) {
	var (
		container *Container
		warnings  []string
		img       *image.Image
		imgID     string
		err       error
	)

	if config.Image != "" {
		img, err = daemon.repositories.LookupImage(config.Image)
		if err != nil {
			return nil, nil, err
		}
		if err = img.CheckDepth(); err != nil {
			return nil, nil, err
		}
		imgID = img.ID
	}

	if warnings, err = daemon.mergeAndVerifyConfig(config, img); err != nil {
		return nil, nil, err
	}
	if !config.NetworkDisabled && daemon.SystemConfig().IPv4ForwardingDisabled {
		warnings = append(warnings, "IPv4 forwarding is disabled.\n")
	}
	if hostConfig == nil {
		hostConfig = &runconfig.HostConfig{}
	}
	if hostConfig.SecurityOpt == nil {
		hostConfig.SecurityOpt, err = daemon.GenerateSecurityOpt(hostConfig.IpcMode, hostConfig.PidMode)
		if err != nil {
			return nil, nil, err
		}
	}
	if container, err = daemon.newContainer(name, config, imgID); err != nil {
		return nil, nil, err
	}
	if err := daemon.Register(container); err != nil {
		return nil, nil, err
	}
	if err := daemon.createRootfs(container); err != nil {
		return nil, nil, err
	}
	if err := daemon.setHostConfig(container, hostConfig); err != nil {
		return nil, nil, err
	}
	if err := container.Mount(); err != nil {
		return nil, nil, err
	}
	defer container.Unmount()

	for spec := range config.Volumes {
		var (
			name, destination string
			parts             = strings.Split(spec, ":")
		)
		switch len(parts) {
		case 2:
			name, destination = parts[0], filepath.Clean(parts[1])
		default:
			name = stringid.GenerateRandomID()
			destination = filepath.Clean(parts[0])
		}
		// Skip volumes for which we already have something mounted on that
		// destination because of a --volume-from.
		if _, mounted := container.MountPoints[destination]; mounted {
			continue
		}
		path, err := container.GetResourcePath(destination)
		if err != nil {
			return nil, nil, err
		}
		if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
			return nil, nil, fmt.Errorf("cannot mount volume over existing file, file exists %s", path)
		}
		v, err := daemon.createVolume(name, config.VolumeDriver)
		if err != nil {
			return nil, nil, err
		}
		rootfs, err := symlink.FollowSymlinkInScope(filepath.Join(container.basefs, destination), container.basefs)
		if err != nil {
			return nil, nil, err
		}
		if path, err = v.Mount(); err != nil {
			return nil, nil, err
		}
		copyExistingContents(rootfs, path)

		container.MountPoints[destination] = &MountPoint{
			Name:        v.Name(),
			Driver:      v.DriverName(),
			Destination: destination,
			RW:          true,
			Volume:      v,
		}
	}
	if err := container.ToDisk(); err != nil {
		return nil, nil, err
	}
	return container, warnings, nil
}

func (daemon *Daemon) GenerateSecurityOpt(ipcMode runconfig.IpcMode, pidMode runconfig.PidMode) ([]string, error) {
	if ipcMode.IsHost() || pidMode.IsHost() {
		return label.DisableSecOpt(), nil
	}
	if ipcContainer := ipcMode.Container(); ipcContainer != "" {
		c, err := daemon.Get(ipcContainer)
		if err != nil {
			return nil, err
		}
		if !c.IsRunning() {
			return nil, fmt.Errorf("cannot join IPC of a non running container: %s", ipcContainer)
		}

		return label.DupSecOpt(c.ProcessLabel), nil
	}
	return nil, nil
}
