// Package pb is a minimal PocketBase client for identity-plane operations:
// authenticate a user, fetch an admin token, list/create/patch/delete users.
// Endpoints mirror PB 0.30 (SPEC-consistent with the Python gateway).
package pb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	credsRefresh func() (string, string)
	BaseURL      string
	HTTP         *http.Client

	mu       sync.Mutex
	adminTok string
	adminAt  time.Time
	Ident    string // superuser identity
	Pass     string // superuser password
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 6 * time.Second},
	}
}

// SetSuperuser configures cached admin-token credentials.
func (c *Client) SetSuperuser(ident, pass string) {
	c.mu.Lock()
	c.Ident, c.Pass = ident, pass
	c.mu.Unlock()
}

func (c *Client) do(method, path string, body any, token string, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("pb: %d %s", e.Status, e.Body)
}

// UserRecord is the subset of a PB users record we use.
type UserRecord struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Verified bool   `json:"verified"`
	Created  string `json:"created"`
}

// Authenticate verifies a username+password (users collection,
// auth-with-password). Returns the record on success.
func (c *Client) Authenticate(username, password string) (*UserRecord, error) {
	var out struct {
		Token  string     `json:"token"`
		Record UserRecord `json:"record"`
	}
	err := c.do("POST", "/api/collections/users/auth-with-password",
		map[string]string{"identity": username, "password": password}, "", &out)
	if err != nil {
		return nil, err
	}
	if out.Record.Username == "" {
		out.Record.Username = username
	}
	if out.Record.Role == "" {
		out.Record.Role = "user"
	}
	return &out.Record, nil
}

// AdminToken returns a cached superuser auth token (refreshed every 20 min,
// mirroring the Python gateway's _pb_admin_token TTL).
// SetCredsRefresh registers a hook re-reading superuser credentials (used
// when the env file appears after boot — fresh-install bootstrap).
func (c *Client) SetCredsRefresh(fn func() (string, string)) {
	c.mu.Lock()
	c.credsRefresh = fn
	c.mu.Unlock()
}

func (c *Client) AdminToken() (string, error) {
	c.mu.Lock()
	ident, pass := c.Ident, c.Pass
	tok, at := c.adminTok, c.adminAt
	refresh := c.credsRefresh
	c.mu.Unlock()
	if tok != "" && time.Since(at) < 20*time.Minute {
		return tok, nil
	}
	if ident == "" && refresh != nil {
		ident, pass = refresh()
		c.mu.Lock()
		c.Ident, c.Pass = ident, pass
		c.mu.Unlock()
	}
	if ident == "" {
		return "", fmt.Errorf("pb: superuser not configured")
	}
	var out struct {
		Token string `json:"token"`
	}
	err := c.do("POST", "/api/collections/_superusers/auth-with-password",
		map[string]string{"identity": ident, "password": pass}, "", &out)
	if err != nil && refresh != nil {
		// Credentials may have been provisioned after boot — refresh once.
		if ident2, pass2 := refresh(); pass2 != "" && pass2 != pass {
			c.mu.Lock()
			c.Ident, c.Pass = ident2, pass2
			c.mu.Unlock()
			err = c.do("POST", "/api/collections/_superusers/auth-with-password",
				map[string]string{"identity": ident2, "password": pass2}, "", &out)
			if err == nil {
				c.mu.Lock()
				c.adminTok, c.adminAt = out.Token, time.Now()
				c.mu.Unlock()
				return out.Token, nil
			}
		}
		return "", err
	}
	c.mu.Lock()
	c.adminTok, c.adminAt = out.Token, time.Now()
	c.mu.Unlock()
	return out.Token, nil
}

// ListUsers fetches all users (perPage=100, single page like Python).
func (c *Client) ListUsers() ([]UserRecord, error) {
	tok, err := c.AdminToken()
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []UserRecord `json:"items"`
	}
	err = c.do("GET", "/api/collections/users/records?perPage=100", nil, tok, &out)
	return out.Items, err
}

// FindUser by username — filter is escaped here (single quotes doubled),
// the single sanctioned interpolation point.
func (c *Client) FindUser(username string) (*UserRecord, error) {
	tok, err := c.AdminToken()
	if err != nil {
		return nil, err
	}
	esc := strings.ReplaceAll(username, "'", "''")
	var out struct {
		Items []UserRecord `json:"items"`
	}
	err = c.do("GET", "/api/collections/users/records?filter="+
		urlQueryEscape("username='"+esc+"'"), nil, tok, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	return &out.Items[0], nil
}

// CreateUser provisions a PB account (passwordConfirm required by PB 0.30).
func (c *Client) CreateUser(username, email, password, role string) (*UserRecord, error) {
	tok, err := c.AdminToken()
	if err != nil {
		return nil, err
	}
	var rec UserRecord
	err = c.do("POST", "/api/collections/users/records", map[string]string{
		"username": username, "email": email,
		"password": password, "passwordConfirm": password,
		"role": role, "emailVisibility": "true",
	}, tok, &rec)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// PatchUser applies partial fields to a user record.
func (c *Client) PatchUser(id string, fields map[string]any) error {
	tok, err := c.AdminToken()
	if err != nil {
		return err
	}
	return c.do("PATCH", "/api/collections/users/records/"+id, fields, tok, nil)
}

// DeleteUser removes a PB account.
func (c *Client) DeleteUser(id string) error {
	tok, err := c.AdminToken()
	if err != nil {
		return err
	}
	return c.do("DELETE", "/api/collections/users/records/"+id, nil, tok, nil)
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(
		strings.ReplaceAll(s, "%", "%25"), " ", "%20"), "'", "%27")
}

// CountUsers: total records in the users collection (0 on fresh install).
func (c *Client) CountUsers() (int64, error) {
	tok, err := c.AdminToken()
	if err != nil {
		return 0, err
	}
	var out struct {
		TotalItems int64 `json:"totalItems"`
	}
	err = c.do("GET", "/api/collections/users/records?perPage=1", nil, tok, &out)
	if err != nil {
		return 0, err
	}
	return out.TotalItems, nil
}
