package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// cloudflareDNS implements dnsProvider with the Cloudflare v4 API. The token
// needs Zone:Read and DNS:Edit on the one zone. Each custom address gets a
// CNAME (name → target) and an SRV record (_minecraft._tcp.name → target:port);
// record IDs are returned in that order.
type cloudflareDNS struct {
	token  string
	zone   string // e.g. voxelport.in
	target string // e.g. play.voxelport.in
	base   string
	client *http.Client

	mu     sync.Mutex
	zoneID string
}

func newCloudflareDNS(token, zone, target string) *cloudflareDNS {
	return &cloudflareDNS{
		token:  token,
		zone:   zone,
		target: target,
		base:   "https://api.cloudflare.com/client/v4",
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	Proxied *bool  `json:"proxied,omitempty"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func (c *cloudflareDNS) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cloudflare %s %s: HTTP %d", method, path, res.StatusCode)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return fmt.Errorf("cloudflare %s %s: %s", method, path, strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *cloudflareDNS) zoneIDFor(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.zoneID
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(c.zone), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("zone %s not found for this token", c.zone)
	}
	c.mu.Lock()
	c.zoneID = zones[0].ID
	c.mu.Unlock()
	return zones[0].ID, nil
}

func (c *cloudflareDNS) fqdn(name string) string { return name + "." + c.zone }
func (c *cloudflareDNS) srvName(name string) string {
	return "_minecraft._tcp." + c.fqdn(name)
}

func (c *cloudflareDNS) srvRecord(name string, port int) cfRecord {
	return cfRecord{
		Type:    "SRV",
		Name:    c.srvName(name),
		TTL:     300,
		Comment: "VoxelPort custom address",
		Data:    map[string]any{"priority": 0, "weight": 5, "port": port, "target": c.target},
	}
}

// Exists reports whether any record already uses name or its SRV name.
func (c *cloudflareDNS) Exists(ctx context.Context, name string) (bool, error) {
	zone, err := c.zoneIDFor(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range []string{c.fqdn(name), c.srvName(name)} {
		var recs []cfRecord
		if err := c.do(ctx, http.MethodGet, "/zones/"+zone+"/dns_records?name="+url.QueryEscape(n), nil, &recs); err != nil {
			return false, err
		}
		if len(recs) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (c *cloudflareDNS) Create(ctx context.Context, name string, port int) ([]string, error) {
	zone, err := c.zoneIDFor(ctx)
	if err != nil {
		return nil, err
	}
	no := false
	var cname cfRecord
	if err := c.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", cfRecord{
		Type: "CNAME", Name: c.fqdn(name), Content: c.target, Proxied: &no, TTL: 300,
		Comment: "VoxelPort custom address",
	}, &cname); err != nil {
		return nil, err
	}
	var srv cfRecord
	if err := c.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", c.srvRecord(name, port), &srv); err != nil {
		// Don't leave half an address behind.
		_ = c.do(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+cname.ID, nil, nil)
		return nil, err
	}
	return []string{cname.ID, srv.ID}, nil
}

func (c *cloudflareDNS) Repoint(ctx context.Context, records []string, name string, port int) error {
	if len(records) < 2 {
		return errors.New("missing SRV record id")
	}
	zone, err := c.zoneIDFor(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/zones/"+zone+"/dns_records/"+records[1], c.srvRecord(name, port), nil)
}

func (c *cloudflareDNS) Delete(ctx context.Context, records []string) error {
	zone, err := c.zoneIDFor(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, id := range records {
		if id == "" {
			continue
		}
		err := c.do(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+id, nil, nil)
		// Already gone (e.g. removed by hand) counts as deleted.
		if err != nil && !strings.Contains(err.Error(), "81044") && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
