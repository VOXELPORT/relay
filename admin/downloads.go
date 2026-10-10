package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// downloads keeps GitHub release download counts (public API, every 15 min)
// and, when Partner Center API credentials are configured, Microsoft Store
// acquisitions per day (every 6 hours; Microsoft's data lags a day or two).
type downloads struct {
	repo, storeApp                 string
	tenant, clientID, clientSecret string
	client                         *http.Client

	mu       sync.Mutex
	github   map[string]any
	ghErr    string
	store    map[string]any
	storeErr string
}

func newDownloads(repo, storeApp, tenant, clientID, secret string) *downloads {
	return &downloads{repo: repo, storeApp: storeApp, tenant: tenant, clientID: clientID, clientSecret: secret,
		client: &http.Client{Timeout: 20 * time.Second}}
}

func (d *downloads) storeConfigured() bool {
	return d.tenant != "" && d.clientID != "" && d.clientSecret != ""
}

func (d *downloads) run() {
	d.fetchGitHub()
	if d.storeConfigured() {
		d.fetchStore()
	}
	gh := time.NewTicker(15 * time.Minute)
	st := time.NewTicker(6 * time.Hour)
	for {
		select {
		case <-gh.C:
			d.fetchGitHub()
		case <-st.C:
			if d.storeConfigured() {
				d.fetchStore()
			}
		}
	}
}

func (d *downloads) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"github": d.github, "github_err": d.ghErr,
		"store": d.store, "store_err": d.storeErr, "store_configured": d.storeConfigured(), "store_app": d.storeApp,
	}
}

func (d *downloads) fetchGitHub() {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+d.repo+"/releases?per_page=50", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "voxelport-admin")
	resp, err := d.client.Do(req)
	if err != nil {
		d.setGitHub(nil, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.setGitHub(nil, "GitHub: "+resp.Status)
		return
	}
	var rels []struct {
		Tag       string    `json:"tag_name"`
		Published time.Time `json:"published_at"`
		Assets    []struct {
			Name  string `json:"name"`
			Count int    `json:"download_count"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		d.setGitHub(nil, err.Error())
		return
	}
	total := 0
	out := []map[string]any{}
	for _, r := range rels {
		n := 0
		assets := []map[string]any{}
		for _, a := range r.Assets {
			// Count installers and archives, not update metadata or checksums.
			if strings.HasSuffix(a.Name, ".yml") || strings.HasSuffix(a.Name, ".blockmap") || strings.HasSuffix(a.Name, ".txt") {
				continue
			}
			n += a.Count
			assets = append(assets, map[string]any{"name": a.Name, "count": a.Count})
		}
		total += n
		out = append(out, map[string]any{"tag": r.Tag, "published": r.Published, "count": n, "assets": assets})
	}
	d.setGitHub(map[string]any{"total": total, "releases": out, "fetched": time.Now()}, "")
}

func (d *downloads) setGitHub(v map[string]any, e string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v != nil {
		d.github = v
	}
	d.ghErr = e
}

// fetchStore reads daily acquisitions from the Partner Center analytics API.
// Setup: register an Azure AD (Entra ID) app, add it to Partner Center under
// Account settings → User management → Azure AD applications with the Manager
// role, and create a client secret for it.
func (d *downloads) fetchStore() {
	tok, err := d.storeToken()
	if err != nil {
		d.setStore(nil, "sign-in failed: "+err.Error())
		return
	}
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -90)
	q := url.Values{
		"applicationId":    {d.storeApp},
		"startDate":        {start.Format("2006-01-02")},
		"endDate":          {end.Format("2006-01-02")},
		"aggregationLevel": {"day"},
		"top":              {"10000"},
	}
	req, _ := http.NewRequest("GET", "https://manage.devcenter.microsoft.com/v1.0/my/analytics/appacquisitions?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.client.Do(req)
	if err != nil {
		d.setStore(nil, err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		d.setStore(nil, fmt.Sprintf("Partner Center: %s %s", resp.Status, truncate(string(body), 200)))
		return
	}
	var parsed struct {
		Value []struct {
			Date     string  `json:"date"`
			Quantity float64 `json:"acquisitionQuantity"`
		} `json:"Value"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		d.setStore(nil, err.Error())
		return
	}
	byDay := map[string]float64{}
	total := 0.0
	for _, v := range parsed.Value {
		day := v.Date
		if len(day) > 10 {
			day = day[:10]
		}
		byDay[day] += v.Quantity
		total += v.Quantity
	}
	d.setStore(map[string]any{"total_90d": total, "days": byDay, "fetched": time.Now()}, "")
}

func (d *downloads) storeToken() (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {d.clientID},
		"client_secret": {d.clientSecret},
		"resource":      {"https://manage.devcenter.microsoft.com"},
	}
	resp, err := d.client.PostForm("https://login.microsoftonline.com/"+url.PathEscape(d.tenant)+"/oauth2/token", form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if out.AccessToken == "" {
		return "", fmt.Errorf("%s", truncate(out.Error, 200))
	}
	return out.AccessToken, nil
}

func (d *downloads) setStore(v map[string]any, e string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v != nil {
		d.store = v
	}
	d.storeErr = e
	if e != "" {
		log.Printf("store analytics: %s", e)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
