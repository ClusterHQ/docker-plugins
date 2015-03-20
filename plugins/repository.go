package plugins

import (
	"errors"
	"fmt"
)

var ErrNotRegistered = errors.New("plugin type is not registered")

type Repository struct {
	plugins map[string]Plugins
}

type Plugins []*Plugin

func (repository *Repository) GetPlugins(kind string) (Plugins, error) {
	plugins, exists := repository.plugins[kind]
	if !exists {
		return nil, ErrNotRegistered
	}
	return plugins, nil
}

var supportedPluginTypes = map[string]struct{}{
	"volume": {},
}

func NewRepository() *Repository {
	return &Repository{
		plugins: make(map[string][]Plugins),
	}
}

func (repository *Repository) RegisterPlugin(addr string) error {
	plugin := &Plugin{addr: addr}
	resp, err := plugin.handshake()
	if err != nil {
		return fmt.Errorf("error in plugin handshake: %v", err)
	}

	for _, interest := range resp.InterestedIn {
		if _, exists := supportedPluginTypes[interest]; !exists {
			return fmt.Errorf("plugin type %s is not supported", interest)
		}

		if _, exists := repository.plugins[interest]; !exists {
			repository.plugins[interest] = []*Plugin{}
		}
		plugin.kind = interest
		repository.plugins[interest] = append(repository.plugins[interest], plugin)
	}

	return nil
}
