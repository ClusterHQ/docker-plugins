package daemon

import (
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/runconfig"
)

type ContainerJSONRaw struct {
	*Container
	HostConfig *runconfig.HostConfig

	// Unused fields for backward compatibility with API versions < 1.12.
	Volumes   map[string]string
	VolumesRW map[string]bool
}

func (daemon *Daemon) ContainerInspectRaw(name string) (*ContainerJSONRaw, error) {
	container, err := daemon.Get(name)
	if err != nil {
		return nil, err
	}

	container.Lock()
	defer container.Unlock()

	return &ContainerJSONRaw{
		Container:  container,
		HostConfig: container.hostConfig,
	}, nil
}

func (daemon *Daemon) ContainerInspect(name string) (*types.ContainerJSON, error) {
	container, err := daemon.Get(name)
	if err != nil {
		return nil, err
	}

	container.Lock()
	defer container.Unlock()

	// make a copy to play with
	hostConfig := *container.hostConfig

	if children, err := daemon.Children(container.Name); err == nil {
		for linkAlias, child := range children {
			hostConfig.Links = append(hostConfig.Links, fmt.Sprintf("%s:%s", child.Name, linkAlias))
		}
	}
	// we need this trick to preserve empty log driver, so
	// container will use daemon defaults even if daemon change them
	if hostConfig.LogConfig.Type == "" {
		hostConfig.LogConfig = daemon.defaultLogConfig
	}

	containerState := &types.ContainerState{
		Running:    container.State.Running,
		Paused:     container.State.Paused,
		Restarting: container.State.Restarting,
		OOMKilled:  container.State.OOMKilled,
		Dead:       container.State.Dead,
		Pid:        container.State.Pid,
		ExitCode:   container.State.ExitCode,
		Error:      container.State.Error,
		StartedAt:  container.State.StartedAt,
		FinishedAt: container.State.FinishedAt,
	}
	volumes := make(map[string]string)
	volumesRW := make(map[string]bool)
	for _, v := range container.volumes {
		config := container.VolumeConfig[v.Name()]
		volumes[config.Destination] = v.Path()
		volumesRW[config.Destination] = config.RW
	}
	for _, b := range container.BindMounts {
		volumes[b.Destination] = b.Source
		volumesRW[b.Destination] = b.RW
	}
	contJSON := &types.ContainerJSON{
		Id:              container.ID,
		Created:         container.Created,
		Path:            container.Path,
		Args:            container.Args,
		Config:          container.Config,
		State:           containerState,
		Image:           container.ImageID,
		NetworkSettings: container.NetworkSettings,
		ResolvConfPath:  container.ResolvConfPath,
		HostnamePath:    container.HostnamePath,
		HostsPath:       container.HostsPath,
		LogPath:         container.LogPath,
		Name:            container.Name,
		RestartCount:    container.RestartCount,
		Driver:          container.Driver,
		ExecDriver:      container.ExecDriver,
		MountLabel:      container.MountLabel,
		ProcessLabel:    container.ProcessLabel,
		Volumes:         volumes,
		VolumesRW:       volumesRW,
		AppArmorProfile: container.AppArmorProfile,
		ExecIDs:         container.GetExecIDs(),
		HostConfig:      &hostConfig,
	}

	return contJSON, nil
}

func (daemon *Daemon) ContainerExecInspect(id string) (*execConfig, error) {
	eConfig, err := daemon.getExecConfig(id)
	if err != nil {
		return nil, err
	}

	return eConfig, nil
}
