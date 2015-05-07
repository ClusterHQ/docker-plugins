package plugins

import plug "github.com/docker/docker/plugins"

// currently created by hand. generation tool would generate this like:
// $ rpc-gen volume/plugins/api.go VolumeDriver > volume/plugins/proxy.go

type VolumeDriverCreateArgs struct {
	Name string
}

type VolumeDriverCreateReturn struct {
	Err error
}

type VolumeDriverRemoveArgs struct {
	Name string
}

type VolumeDriverRemoveReturn struct {
	Err error
}

type VolumeDriverPathArgs struct {
	Name string
}

type VolumeDriverPathReturn struct {
	Mountpoint string
	Err        error
}

type VolumeDriverMountArgs struct {
	Name string
}

type VolumeDriverMountReturn struct {
	Mountpoint string
	Err        error
}

type VolumeDriverUnmountArgs struct {
	Name string
}

type VolumeDriverUnmountReturn struct {
	Err error
}

type volumeDriverProxy struct {
	client *plug.Client
}

func (pp *volumeDriverProxy) Create(name string) error {
	args := VolumeDriverCreateArgs{name}
	var ret VolumeDriverCreateReturn
	err := pp.client.Call("VolumeDriver.Create", args, &ret)
	if err != nil {
		return err
	}
	return ret.Err
}

func (pp *volumeDriverProxy) Remove(name string) error {
	args := VolumeDriverRemoveArgs{name}
	var ret VolumeDriverRemoveReturn
	err := pp.client.Call("VolumeDriver.Remove", args, &ret)
	if err != nil {
		return err
	}
	return ret.Err
}

func (pp *volumeDriverProxy) Path(name string) (string, error) {
	args := VolumeDriverPathArgs{name}
	var ret VolumeDriverPathReturn
	if err := pp.client.Call("VolumeDriver.Path", args, &ret); err != nil {
		return "", err
	}
	return ret.Mountpoint, ret.Err
}

func (pp *volumeDriverProxy) Mount(name string) (mountpoint string, err error) {
	args := VolumeDriverMountArgs{name}
	var ret VolumeDriverMountReturn
	if err = pp.client.Call("VolumeDriver.Mount", args, &ret); err != nil {
		return "", err
	}
	return ret.Mountpoint, ret.Err
}

func (pp *volumeDriverProxy) Unmount(name string) error {
	args := VolumeDriverUnmountArgs{name}
	var ret VolumeDriverUnmountReturn
	err := pp.client.Call("VolumeDriver.Unmount", args, &ret)
	if err != nil {
		return err
	}
	return ret.Err
}
