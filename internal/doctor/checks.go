package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/workflow"
	"gopkg.in/yaml.v3"
)

const (
	dockerTimeout = 5 * time.Second
	gitTimeout    = 3 * time.Second
	limaTimeout   = 5 * time.Second
	ghTimeout     = 8 * time.Second
	xcodeTimeout  = 5 * time.Second
)

// versionToken finds the first dotted version number in free-form tool
// output, e.g. "limactl version 2.1.0" or "Command Line Tools" package info.
var versionToken = regexp.MustCompile(`\d+(\.\d+)+`)

// majorOf returns the leading major version number found anywhere in s.
func majorOf(s string) (int, bool) {
	tok := versionToken.FindString(s)
	if tok == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.SplitN(tok, ".", 2)[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

func checkDocker(ctx context.Context, env Env) []Result {
	_, stderr, code, err := env.Run(ctx, dockerTimeout, "docker", "info", "--format", "{{.OperatingSystem}}")
	if err != nil || code != 0 {
		detail := "Docker is not reachable"
		switch {
		case err != nil:
			detail = "Docker is not reachable: " + err.Error()
		case strings.TrimSpace(stderr) != "":
			detail = "Docker is not reachable: " + strings.TrimSpace(stderr)
		}
		return []Result{{Name: "docker", Status: Failed, Detail: detail, Fix: "start Docker Desktop or OrbStack, then retry"}}
	}
	docker := Result{Name: "docker", Status: OK, Detail: "Docker is reachable"}

	out, stderr, code, err := env.Run(ctx, dockerTimeout, "docker", "compose", "version", "--short")
	if err != nil || code != 0 {
		detail := "docker compose v2 is not available"
		if err != nil {
			detail = "docker compose v2 is not available: " + err.Error()
		} else if strings.TrimSpace(stderr) != "" {
			detail = "docker compose v2 is not available: " + strings.TrimSpace(stderr)
		}
		return []Result{docker, {Name: "docker-compose", Status: Failed, Detail: detail, Fix: "install the Docker Compose v2 (or newer) plugin"}}
	}
	v := strings.TrimSpace(out)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	major, ok := majorOf(v)
	if !ok || major < 2 {
		return []Result{docker, {Name: "docker-compose", Status: Failed, Detail: fmt.Sprintf("docker compose %s is too old (2.x or newer is required)", v), Fix: "upgrade to Docker Compose v2 or newer"}}
	}
	return []Result{docker, {Name: "docker-compose", Status: OK, Detail: fmt.Sprintf("docker compose %s", v)}}
}

func checkGit(ctx context.Context, env Env) Result {
	out, _, code, err := env.Run(ctx, gitTimeout, "git", "--version")
	if err != nil || code != 0 {
		return Result{Name: "git", Status: Failed, Detail: "git is not available", Fix: "install git and ensure it is on PATH"}
	}
	return Result{Name: "git", Status: OK, Detail: strings.TrimSpace(out)}
}

func checkLima(ctx context.Context, env Env) Result {
	const fix = "install Lima 2.x (`brew install lima`) -- only needed to run workflow revisions"
	out, _, code, err := env.Run(ctx, limaTimeout, "limactl", "--version")
	if err != nil || code != 0 {
		return Result{Name: "lima", Status: Warning, Detail: "Lima is not installed", Fix: fix}
	}
	major, ok := majorOf(out)
	if !ok || major != 2 {
		return Result{Name: "lima", Status: Warning, Detail: fmt.Sprintf("%s does not report version 2.x", strings.TrimSpace(out)), Fix: fix}
	}
	return Result{Name: "lima", Status: OK, Detail: strings.TrimSpace(out)}
}

func checkGH(ctx context.Context, env Env) Result {
	const fix = "install the GitHub CLI and run `gh auth login` -- only needed for pull-request/publication output"
	out, stderr, code, err := env.Run(ctx, ghTimeout, "gh", "auth", "status")
	if err != nil {
		return Result{Name: "gh", Status: Warning, Detail: "gh is not installed", Fix: fix}
	}
	if code != 0 {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(out)
		}
		if detail == "" {
			detail = "gh is not authenticated"
		}
		return Result{Name: "gh", Status: Warning, Detail: detail, Fix: fix}
	}
	return Result{Name: "gh", Status: OK, Detail: "gh is authenticated"}
}

// checkManifest resolves the worktree root via the injected Runner (mirroring
// feature.Toplevel's own git probe, kept on the Runner seam so this check
// stays testable without a real git binary), then reports whether
// envctl.yaml exists and parses. It returns the true on-disk manifest
// version -- never workflow.Load's Config.Version, which is hard-coded to 2
// even for legacy manifests -- so callers can gate the credential check on
// it accurately.
func checkManifest(ctx context.Context, env Env) (result Result, cfg *workflow.Config, rawVersion int) {
	out, _, code, err := env.Run(ctx, gitTimeout, "git", "-C", env.Dir, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return Result{Name: "envctl.yaml", Status: Warning, Detail: "not inside a git repository", Fix: "run envctl doctor from inside a git worktree"}, nil, 0
	}
	root := strings.TrimSpace(out)

	raw, err := os.ReadFile(filepath.Join(root, manifest.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Result{Name: "envctl.yaml", Status: Warning, Detail: "envctl.yaml not found at " + root, Fix: "run `envctl init --project <prefix>`"}, nil, 0
	}
	if err != nil {
		return Result{Name: "envctl.yaml", Status: Failed, Detail: err.Error(), Fix: "fix the permissions or path of envctl.yaml"}, nil, 0
	}

	var header struct {
		Version int `yaml:"version"`
	}
	_ = yaml.Unmarshal(raw, &header)
	rawVersion = header.Version
	if rawVersion == 0 {
		rawVersion = 1 // manifest.Load's own default; matches on-disk convention
	}

	c, err := workflow.Load(root)
	if err != nil {
		return Result{Name: "envctl.yaml", Status: Failed, Detail: err.Error(), Fix: "fix envctl.yaml: " + err.Error()}, nil, rawVersion
	}
	return Result{Name: "envctl.yaml", Status: OK, Detail: fmt.Sprintf("found and parses (version %d)", rawVersion)}, &c, rawVersion
}

// checkCredential reports whether one role's agent credential resolves,
// without ever printing the credential value: only the (already redacted)
// error from agent.ResolveCredential is formatted.
func checkCredential(name, root string, h workflow.Harness) Result {
	if _, err := agent.ResolveCredential(root, h.Kind, h.Credential); err != nil {
		return Result{Name: name, Status: Failed, Detail: fmt.Sprintf("%s credential does not resolve", h.Kind), Fix: err.Error()}
	}
	return Result{Name: name, Status: OK, Detail: fmt.Sprintf("%s credential resolves", h.Kind)}
}

// checkXcodeCLT reports nothing on non-macOS. On macOS it checks not only
// whether the Command Line Tools are installed but whether Homebrew will
// accept them: a CLT major version older than the running macOS major
// version is refused by Homebrew even though xcode-select reports them
// installed.
func checkXcodeCLT(ctx context.Context, env Env) []Result {
	if env.GOOS != "darwin" {
		return nil
	}
	if _, _, code, err := env.Run(ctx, xcodeTimeout, "xcode-select", "-p"); err != nil || code != 0 {
		return []Result{{Name: "xcode-clt", Status: Failed, Detail: "Xcode Command Line Tools are not installed", Fix: "run `xcode-select --install`"}}
	}

	const reinstall = "sudo rm -rf /Library/Developer/CommandLineTools && sudo xcode-select --install"
	cltMajor, cltOK := cltVersion(ctx, env)
	macMajor, macOK := macOSVersion(ctx, env)
	if !cltOK || !macOK {
		return []Result{{Name: "xcode-clt", Status: Warning, Detail: "could not determine the installed Command Line Tools version", Fix: "run `xcode-select --install` to reinstall, or check manually with `pkgutil --pkg-info=com.apple.pkg.CLTools_Executables`"}}
	}
	if cltMajor < macMajor {
		return []Result{{Name: "xcode-clt", Status: Failed, Detail: fmt.Sprintf("Command Line Tools version %d is older than macOS %d; Homebrew will refuse to install", cltMajor, macMajor), Fix: reinstall}}
	}
	return []Result{{Name: "xcode-clt", Status: OK, Detail: fmt.Sprintf("Command Line Tools %d installed for macOS %d", cltMajor, macMajor)}}
}

// cltVersion prefers the package receipt pkgutil tracks for a standalone CLT
// install, falling back to the version recorded for the CommandLineTools
// directory itself (e.g. when Xcode.app provided the tools without a
// separate pkg receipt).
func cltVersion(ctx context.Context, env Env) (int, bool) {
	if out, _, code, err := env.Run(ctx, xcodeTimeout, "pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"); err == nil && code == 0 {
		if m := pkgutilVersion.FindStringSubmatch(out); m != nil {
			if major, ok := majorOf(m[1]); ok {
				return major, true
			}
		}
	}
	if out, _, code, err := env.Run(ctx, xcodeTimeout, "defaults", "read", "/Library/Developer/CommandLineTools/version.plist", "CFBundleShortVersionString"); err == nil && code == 0 {
		if major, ok := majorOf(strings.TrimSpace(out)); ok {
			return major, true
		}
	}
	return 0, false
}

var pkgutilVersion = regexp.MustCompile(`(?m)^\s*version:\s*(\S+)`)

func macOSVersion(ctx context.Context, env Env) (int, bool) {
	out, _, code, err := env.Run(ctx, xcodeTimeout, "sw_vers", "-productVersion")
	if err != nil || code != 0 {
		return 0, false
	}
	return majorOf(strings.TrimSpace(out))
}
