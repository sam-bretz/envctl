package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

type DataConfig struct {
	Datasets []Dataset `yaml:"datasets" json:"datasets"`
	Quiesce  []string  `yaml:"quiesce,omitempty" json:"quiesce,omitempty"`
}

type Dataset struct {
	ID             string `yaml:"id" json:"id"`
	Adapter        string `yaml:"adapter" json:"adapter"`
	Service        string `yaml:"service" json:"service"`
	Database       string `yaml:"database" json:"database"`
	User           string `yaml:"user" json:"user"`
	SeedRepository string `yaml:"seed_repository" json:"seed_repository"`
	SeedFile       string `yaml:"seed_file" json:"seed_file"`
	VerifySQL      string `yaml:"verify_sql" json:"verify_sql"`
	VerifyEquals   string `yaml:"verify_equals" json:"verify_equals"`
	VerifyKey      string `yaml:"verify_key,omitempty" json:"verify_key,omitempty"`
}

type DatasetSnapshot struct {
	Adapter       string   `json:"adapter"`
	BindingDigest string   `json:"binding_digest"`
	Format        string   `json:"format"`
	ToolVersion   string   `json:"tool_version"`
	Source        Artifact `json:"source"`
	Evidence      Artifact `json:"evidence"`
}

var databaseIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
var serviceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var fixtureKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (d Dataset) SnapshotFormat() string {
	switch d.Adapter {
	case "postgres":
		return "postgres-custom-v1"
	case "http-fixture":
		return "envctl-http-fixture-v1"
	default:
		return ""
	}
}
func (d Dataset) SnapshotMedia() string {
	switch d.Adapter {
	case "postgres":
		return "application/vnd.envctl.postgres-dump"
	case "http-fixture":
		return "application/vnd.envctl.http-fixture+json"
	default:
		return ""
	}
}
func (d Dataset) SeedMedia() string {
	if d.Adapter == "http-fixture" {
		return "application/json"
	}
	return "application/sql"
}

func (d Dataset) Validate() error {
	if !identifier.MatchString(d.ID) || !serviceIdentifier.MatchString(d.Service) {
		return errors.New("dataset requires valid ID and Compose service")
	}
	if !identifier.MatchString(d.SeedRepository) || d.SeedFile == "" || path.IsAbs(d.SeedFile) || path.Clean(d.SeedFile) != d.SeedFile || d.SeedFile == ".." || strings.HasPrefix(d.SeedFile, "../") || strings.ContainsAny(d.SeedFile, "\x00\r\n") {
		return errors.New("dataset seed must be a relative file in a pinned repository")
	}
	switch d.Adapter {
	case "postgres":
		if !databaseIdentifier.MatchString(d.Database) || !databaseIdentifier.MatchString(d.User) || d.Database == "postgres" || d.Database == "template0" || d.Database == "template1" {
			return errors.New("dataset needs an application database and a local database role")
		}
		if strings.TrimSpace(d.VerifySQL) == "" || strings.TrimSpace(d.VerifyEquals) == "" || d.VerifyKey != "" {
			return errors.New("PostgreSQL dataset requires verification SQL and expected output, without verify_key")
		}
	case "http-fixture":
		if !fixtureKey.MatchString(d.VerifyKey) || !json.Valid([]byte(d.VerifyEquals)) || d.Database != "" || d.User != "" || d.VerifySQL != "" {
			return errors.New("HTTP fixture requires verify_key and JSON verify_equals, without database, user or verify_sql")
		}
	default:
		return fmt.Errorf("unsupported dataset adapter %q", d.Adapter)
	}
	return nil
}

func (d DataConfig) Validate(repos []Repository) error {
	seen := map[string]bool{}
	targets := map[string]bool{}
	for _, dataset := range d.Datasets {
		if err := dataset.Validate(); err != nil {
			return err
		}
		if seen[dataset.ID] {
			return errors.New("duplicate dataset ID")
		}
		seen[dataset.ID] = true
		target := dataset.Service + "/" + dataset.Database
		if targets[target] {
			return errors.New("datasets must not alias the same data target")
		}
		targets[target] = true
		found := false
		for _, repo := range repos {
			if repo.ID == dataset.SeedRepository {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("dataset %s seed repository is unavailable", dataset.ID)
		}
	}
	for _, service := range d.Quiesce {
		if !serviceIdentifier.MatchString(service) {
			return errors.New("invalid dataset quiescence service")
		}
	}
	if len(d.Datasets) > 1 && len(d.Quiesce) == 0 {
		return errors.New("multiple datasets require an explicit writer quiescence procedure")
	}
	return nil
}
