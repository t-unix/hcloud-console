package main

import (
	"encoding/json"
	"fmt"
	"os/exec"

	fzf "github.com/koki-develop/go-fzf"
)

// serverEntry mirrors the fields of `hcloud server list -o json` we
// actually display in the picker.
type serverEntry struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	PublicNet  struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Datacenter struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
}

// listServers shells out to the hcloud CLI to fetch the user's server
// list. We rely on hcloud already being authenticated.
func listServers() ([]serverEntry, error) {
	out, err := exec.Command("hcloud", "server", "list", "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("hcloud server list: %w", err)
	}
	var servers []serverEntry
	if err := json.Unmarshal(out, &servers); err != nil {
		return nil, fmt.Errorf("parse server list: %w", err)
	}
	return servers, nil
}

// selectServer fetches the server list and shows a fuzzy-finder UI for
// the user to pick one. Returns the chosen server's name.
func selectServer() (string, error) {
	servers, err := listServers()
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "", fmt.Errorf("no servers in this hcloud project")
	}

	items := make([]string, len(servers))
	for i, s := range servers {
		items[i] = formatServerEntry(s)
	}

	f, err := fzf.New(
		fzf.WithPrompt("server> "),
		fzf.WithNoLimit(false),
	)
	if err != nil {
		return "", fmt.Errorf("init fzf: %w", err)
	}
	idxs, err := f.Find(items, func(i int) string { return items[i] })
	if err != nil {
		return "", fmt.Errorf("fuzzy find: %w", err)
	}
	if len(idxs) == 0 {
		return "", fmt.Errorf("no server selected")
	}
	return servers[idxs[0]].Name, nil
}

func formatServerEntry(s serverEntry) string {
	return fmt.Sprintf("%-20s  %-9d  %-10s  %-15s  %-7s  %s",
		s.Name, s.ID, s.Status, s.PublicNet.IPv4.IP,
		s.ServerType.Name, s.Datacenter.Location.Name)
}
