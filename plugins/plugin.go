package plugins

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker/utils"
)

type Plugin interface {
	Activate() (Manifest, error)
}

type Manifest struct {
	Extensions []string
}

type RemotePlugin struct {
	Name string
	Addr string
}

func (p *RemotePlugin) Activate() (m Manifest, err error) {
	tr := &http.Transport{}
	protoAndAddr := strings.Split(p.Addr, "://")
	utils.ConfigureTCPTransport(tr, protoAndAddr[0], protoAndAddr[1])

	client := &http.Client{Transport: tr} // FIXME: TLS? :scream:

	res, err := client.PostForm(p.activateURL(), url.Values{})
	if err != nil {
		return m, err
	}
	if res.StatusCode != http.StatusOK {
		return m, fmt.Errorf("Request failed: %v", res.StatusCode)
	}

	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return m, err
	}

	if len(m.Extensions) == 0 {
		return m, fmt.Errorf("No extension points")
	}

	return m, nil
}

func (p *RemotePlugin) activateURL() string {
	u, _ := url.Parse(p.Addr)
	u.Path = "Plugin.Activate" // :pensive:

	return u.String()
}
