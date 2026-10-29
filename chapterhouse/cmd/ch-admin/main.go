// Command ch-admin manages chapterhouse users and API keys.
//
// Every command except bootstrap calls ch-api over HTTP with an API key
// (CHAPTERHOUSE_URL, CHAPTERHOUSE_API_KEY) and prints the JSON response.
// bootstrap talks to Postgres directly (DATABASE_* env, as ch-api) to
// create the first admin and its key, since no key exists yet.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/logan-broit/ghola/chapterhouse/internal/auth"
	"github.com/logan-broit/ghola/chapterhouse/internal/config"
	"github.com/logan-broit/ghola/chapterhouse/internal/repository/sqlc"
)

const usage = `usage: ch-admin <command> [flags] [key=value ...]

  bootstrap --username NAME [--key-name NAME]   first admin + key (direct DB)
  me
  keys list [--all | --user ID]
  keys create --name NAME [--user ID] [--expires 90d]
  keys revoke ID [--admin]
  users list
  users get|deactivate|reactivate ID
  users create --username NAME [--email E] [--display-name D] [--admin]
  users update ID [--username N] [--email E] [--display-name D] [--admin=true|false]
  audit [--all]
  stats

Trailing key=value pairs become query parameters on list commands
(limit=, offset=, active=true, user_id=, action=, resource_type=).

Env: CHAPTERHOUSE_URL (default http://localhost:8080), CHAPTERHOUSE_API_KEY.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ch-admin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "bootstrap":
		return bootstrap(args)
	case "me":
		return call(http.MethodGet, "/v1/me", nil)
	case "stats":
		return call(http.MethodGet, "/v1/admin/stats", nil)
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		all := fs.Bool("all", false, "every user's events (admin)")
		_ = fs.Parse(args)
		path := "/v1/audit"
		if *all {
			path = "/v1/admin/audit"
		}
		return call(http.MethodGet, path+query(fs.Args()), nil)
	case "keys":
		return keys(args)
	case "users":
		return users(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func keys(args []string) error {
	if len(args) == 0 {
		return errors.New("keys: want list, create or revoke")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("keys "+sub, flag.ExitOnError)
	switch sub {
	case "list":
		all := fs.Bool("all", false, "every user's keys (admin)")
		user := fs.String("user", "", "one user's keys (admin)")
		_ = fs.Parse(args)
		path := "/v1/keys"
		switch {
		case *user != "":
			path = "/v1/admin/users/" + url.PathEscape(*user) + "/keys"
		case *all:
			path = "/v1/admin/keys"
		}
		return call(http.MethodGet, path+query(fs.Args()), nil)
	case "create":
		name := fs.String("name", "", "key name (required)")
		user := fs.String("user", "", "create for another user (admin)")
		expires := fs.String("expires", "", "lifetime, e.g. 30d, 90d, 1y")
		_ = fs.Parse(args)
		if *name == "" {
			return errors.New("keys create: --name is required")
		}
		path := "/v1/keys"
		if *user != "" {
			path = "/v1/admin/users/" + url.PathEscape(*user) + "/keys"
		}
		return call(http.MethodPost, path, map[string]string{"name": *name, "expires_in": *expires})
	case "revoke":
		admin := fs.Bool("admin", false, "revoke any user's key (admin)")
		id, rest := popID(args)
		_ = fs.Parse(rest)
		if id == "" {
			return errors.New("keys revoke: key ID required")
		}
		path := "/v1/keys/"
		if *admin {
			path = "/v1/admin/keys/"
		}
		return call(http.MethodDelete, path+url.PathEscape(id), nil)
	}
	return fmt.Errorf("keys: unknown subcommand %q", sub)
}

func users(args []string) error {
	if len(args) == 0 {
		return errors.New("users: want list, get, create, update, deactivate or reactivate")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("users "+sub, flag.ExitOnError)
	const base = "/v1/admin/users"
	switch sub {
	case "list":
		_ = fs.Parse(args)
		return call(http.MethodGet, base+query(fs.Args()), nil)
	case "create":
		username := fs.String("username", "", "username (required)")
		email := fs.String("email", "", "email")
		display := fs.String("display-name", "", "display name")
		admin := fs.Bool("admin", false, "grant admin")
		_ = fs.Parse(args)
		if *username == "" {
			return errors.New("users create: --username is required")
		}
		return call(http.MethodPost, base, map[string]any{
			"username": *username, "email": *email, "display_name": *display, "is_admin": *admin,
		})
	case "update":
		id, rest := popID(args)
		username := fs.String("username", "", "new username")
		email := fs.String("email", "", "new email")
		display := fs.String("display-name", "", "new display name")
		admin := fs.String("admin", "", "true or false")
		_ = fs.Parse(rest)
		if id == "" {
			return errors.New("users update: user ID required")
		}
		body := map[string]any{}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "username":
				body["username"] = *username
			case "email":
				body["email"] = *email
			case "display-name":
				body["display_name"] = *display
			case "admin":
				body["is_admin"] = *admin == "true"
			}
		})
		return call(http.MethodPut, base+"/"+url.PathEscape(id), body)
	case "get", "deactivate", "reactivate":
		id, _ := popID(args)
		if id == "" {
			return fmt.Errorf("users %s: user ID required", sub)
		}
		path := base + "/" + url.PathEscape(id)
		switch sub {
		case "get":
			return call(http.MethodGet, path, nil)
		case "deactivate":
			return call(http.MethodDelete, path, nil)
		default:
			return call(http.MethodPost, path+"/reactivate", nil)
		}
	}
	return fmt.Errorf("users: unknown subcommand %q", sub)
}

// popID takes a leading positional argument so flags may follow it.
func popID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// query turns trailing key=value arguments into a query string.
func query(args []string) string {
	q := url.Values{}
	for _, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// call sends one request to ch-api and prints the JSON response.
func call(method, path string, body any) error {
	key := os.Getenv("CHAPTERHOUSE_API_KEY")
	if key == "" {
		return errors.New("CHAPTERHOUSE_API_KEY is not set (run bootstrap for the first key)")
	}
	base := strings.TrimRight(os.Getenv("CHAPTERHOUSE_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(out))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(out)
	}
	fmt.Println(strings.TrimSpace(pretty.String()))
	return nil
}

// bootstrap creates (or reuses) an admin user and mints a key for it,
// straight in Postgres. Rerunning it for the same user mints another key,
// which is also the recovery path for a lost admin key.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	username := fs.String("username", "", "admin username (required)")
	keyName := fs.String("key-name", "bootstrap", "name for the new key")
	_ = fs.Parse(args)
	if *username == "" {
		return errors.New("bootstrap: --username is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	q := sqlc.New(pool)

	user, err := q.GetUserByUsername(ctx, *username)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		user, err = q.CreateUserWithPassword(ctx, sqlc.CreateUserWithPasswordParams{
			ID: uuid.New(), Username: *username, IsAdmin: true, Metadata: []byte("{}"),
		})
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
	case err != nil:
		return fmt.Errorf("look up user: %w", err)
	case !user.IsAdmin:
		return fmt.Errorf("user %q exists and is not an admin", *username)
	}

	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		return err
	}
	key, err := q.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		UserID: user.ID, KeyHash: hash, KeyPrefix: prefix, Name: *keyName,
	})
	if err != nil {
		return fmt.Errorf("create key: %w", err)
	}
	_, _ = q.CreateAuditLog(ctx, sqlc.CreateAuditLogParams{
		UserID:       pgtype.UUID{Bytes: user.ID, Valid: true},
		Action:       "create",
		ResourceType: "api_key",
		ResourceID:   pgtype.Text{String: key.ID.String(), Valid: true},
		UserAgent:    pgtype.Text{String: "ch-admin bootstrap", Valid: true},
	})

	return json.NewEncoder(os.Stdout).Encode(map[string]string{
		"user_id": user.ID.String(), "username": user.Username, "key_id": key.ID.String(), "key": plaintext,
	})
}
