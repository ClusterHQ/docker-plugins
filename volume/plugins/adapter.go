package plugins

type volumeDriverAdapter struct {
	name  string
	proxy *volumeDriverProxy
}

func (a *volumeDriverAdapter) Name() string {
	return a.name
}

func (a *volumeDriverAdapter) Create(name string) (Volume, error) {
	err := a.proxy.Create(name)
	if err != nil {
		return nil, err
	}
	return &volumeAdapter{a.proxy, name}, nil
}

func (a *volumeDriverAdapter) Remove(volume Volume) error {
	return a.proxy.Remove(volume.Name())
}

type volumeAdapter struct {
	proxy *volumeDriverProxy
	name  string
}

func (a *volumeAdapter) Name() string {
	return name
}
func (a *volumeAdapter) Mount() (string, error) {
	return a.proxy.Mount(a.name)
}
func (a *volumeAdapter) Unmount() error {
	return a.proxy.Unmount(a.name)
}
