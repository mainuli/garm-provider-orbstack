package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
)

const profileName = "garm-orbstack"
const recoveryHint = "recover with garm-orbstack garm-cli profile add --name garm-orbstack --url https://localhost:9997 (missing profile), or profile login (expired token)"

type cliProfile struct {
	Active   string `toml:"active_manager"`
	Managers []struct {
		Name  string `toml:"name"`
		URL   string `toml:"base_url"`
		Token string `toml:"bearer_token"`
	} `toml:"manager"`
}

func profilePath(p Paths) string {
	return filepath.Join(p.CLIHome, ".local/share/garm-cli/config.toml")
}

func validateProfile(p Paths, requireToken bool) error {
	path := profilePath(p)
	if err := rejectSymlinks(p.Home, path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("managed profile unavailable; %s: %w", recoveryHint, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("managed CLI profile must have mode 0600")
	}
	var profile cliProfile
	if _, err := toml.DecodeFile(path, &profile); err != nil {
		return fmt.Errorf("invalid managed profile: %w", err)
	}
	if len(profile.Managers) != 1 || profile.Active != profileName || profile.Managers[0].Name != profileName || profile.Managers[0].URL != controllerURL {
		return fmt.Errorf("managed profile must exclusively target %s as %s; %s", controllerURL, profileName, recoveryHint)
	}
	if requireToken && profile.Managers[0].Token == "" {
		return fmt.Errorf("managed profile is not authenticated; %s", recoveryHint)
	}
	return nil
}

func prepareCLIHome(p Paths) error {
	for _, dir := range []string{p.CLIHome, filepath.Join(p.CLIHome, ".local"), filepath.Join(p.CLIHome, ".local/share"), filepath.Dir(profilePath(p))} {
		if err := rejectSymlinks(p.Home, dir); err != nil {
			return err
		}
		if err := privateDir(dir); err != nil {
			return err
		}
	}
	path := profilePath(p)
	if err := rejectSymlinks(p.Home, path); err != nil {
		return err
	}
	// Upstream SaveConfig uses os.Create (0666); precreate mode 0600 so an
	// interactive CLI never exposes a bearer token even before it returns.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(f.Chmod(0o600), f.Close())
}

func cliCommand(ctx context.Context, p Paths, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, filepath.Join(p.Release, "garm-cli"), args...)
	cmd.Env = cleanEnvironment(p.CLIHome, p.CA)
	return cmd
}

func cliInteractive(ctx context.Context, p Paths, args ...string) error {
	if err := prepareCLIHome(p); err != nil {
		return err
	}
	cmd := cliCommand(ctx, p, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("garm-cli failed (credentials and command output not captured): %w", err)
	}
	return nil
}

func cliJSON(ctx context.Context, p Paths, result any, args ...string) error {
	if err := validateProfile(p, true); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := cliCommand(ctx, p, append([]string{"--format", "json"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("managed CLI request failed; %s: %w", recoveryHint, err)
	}
	if err := json.Unmarshal(out, result); err != nil {
		return fmt.Errorf("invalid managed CLI JSON response: %w", err)
	}
	return nil
}

type controllerInfo struct {
	ID          string `json:"controller_id"`
	CA          []byte `json:"ca_cert_bundle"`
	MetadataURL string `json:"metadata_url"`
	CallbackURL string `json:"callback_url"`
}

func readController(ctx context.Context, p Paths, expectedID string) (controllerInfo, error) {
	var info controllerInfo
	if err := validateProfile(p, true); err != nil {
		return info, err
	}
	if err := cliJSON(ctx, p, &info, "controller", "show"); err != nil {
		return info, err
	}
	if _, err := uuid.Parse(info.ID); err != nil {
		return info, errors.New("controller returned an invalid UUID")
	}
	if expectedID != "" && info.ID != expectedID {
		return info, errors.New("managed profile belongs to a different controller; refusing operation")
	}
	return info, nil
}

func validateControllerCA(p Paths, info controllerInfo) error {
	ca, err := os.ReadFile(p.CA)
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(ca), bytes.TrimSpace(info.CA)) {
		return errors.New("controller stored CA bundle differs from installation CA; runners cannot trust metadata/callback TLS")
	}
	if info.MetadataURL != guestURL+"/api/v1/metadata" || info.CallbackURL != guestURL+"/api/v1/callbacks" {
		return errors.New("controller metadata/callback URLs differ from the managed isolated-guest endpoints")
	}
	return nil
}

// errProviderNotRegistered is returned ONLY when the controller answered
// `provider list` successfully and the orbstack entry is absent. It is the
// sole condition that may justify restarting the controller: a query
// failure (timeout, TLS, auth, malformed output) must NEVER restart,
// because a kickstart -k kills the controller's process group including
// any in-flight provider clone holding its inherited lock.
var errProviderNotRegistered = errors.New("controller has not registered the orbstack external provider")

// ensureProvider verifies the provider registration. Only a PROVEN absent
// registration (successful provider-list response without the orbstack
// entry — i.e. the running controller predates the on-disk provider
// config after an interrupted install) triggers one restart and re-check.
// Query errors fail without touching the running controller.
func ensureProvider(ctx context.Context, launchctl string, p Paths) error {
	err := checkProvider(ctx, p)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errProviderNotRegistered) {
		return err
	}
	if _, restartErr := startService(ctx, launchctl, p, true); restartErr != nil {
		return errors.Join(err, restartErr)
	}
	if _, waitErr := waitController(ctx, p); waitErr != nil {
		return errors.Join(err, waitErr)
	}
	return checkProvider(ctx, p)
}

func checkProvider(ctx context.Context, p Paths) error {
	var providers []struct {
		Name string `json:"name"`
	}
	if err := cliJSON(ctx, p, &providers, "provider", "list"); err != nil {
		return fmt.Errorf("querying controller providers (no restart): %w", err)
	}
	for _, provider := range providers {
		if provider.Name == "orbstack" {
			return nil
		}
	}
	return errProviderNotRegistered
}

type commandClass struct {
	Recovery, Login bool
	Args            []string
	Action          string
}

func classifyCLI(args []string) (commandClass, error) {
	class := commandClass{Args: append([]string(nil), args...)}
	var words []string
	format := "table"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--debug" || strings.HasPrefix(arg, "--debug=") {
			return class, errors.New("debug HTTP logging is disabled to protect managed bearer tokens")
		}
		if arg == "--format" {
			i++
			if i >= len(args) {
				return class, errors.New("--format requires a value")
			}
			format = args[i]
			continue
		}
		if strings.HasPrefix(arg, "--format=") {
			format = strings.TrimPrefix(arg, "--format=")
			continue
		}
		words = append(words, arg)
	}
	if len(words) == 0 {
		return class, nil
	}
	if strings.HasPrefix(words[0], "-") {
		return class, errors.New("unsupported global CLI option")
	}
	if words[0] == "init" {
		return class, errors.New("controller init is installer-only; use profile add/login for recovery")
	}
	if words[0] != "profile" {
		return class, nil
	}
	if len(words) < 2 {
		return class, errors.New("specify profile add, login, list, switch, or delete")
	}
	class.Recovery, class.Action = true, words[1]
	switch class.Action {
	case "add", "create":
		class.Login = true
		class.Action = "add"
	case "login":
		class.Login = true
	case "list", "ls":
		class.Action = "list"
	case "delete", "remove", "rm", "del":
		class.Action = "delete"
	case "switch":
	default:
		return class, errors.New("unsupported profile recovery command")
	}
	if class.Action == "list" && format != "table" {
		return class, errors.New("profile JSON output exposes bearer tokens; use table format")
	}
	seen := make(map[string]bool)
	positionals := 0
	for i := 2; i < len(words); i++ {
		arg := words[i]
		if !strings.HasPrefix(arg, "-") {
			positionals++
			if (class.Action != "delete" && class.Action != "switch") || arg != profileName || positionals > 1 {
				return class, errors.New("only the managed garm-orbstack profile may be selected")
			}
			continue
		}
		key, value, hasValue := strings.Cut(arg, "=")
		switch key {
		case "-n":
			key = "--name"
		case "-a":
			key = "--url"
		case "-u":
			key = "--username"
		}
		if key != "--name" && key != "--url" && key != "--username" {
			return class, fmt.Errorf("unsupported recovery flag %s (passwords must be entered interactively)", key)
		}
		if seen[key] {
			return class, fmt.Errorf("duplicate recovery flag %s", key)
		}
		seen[key] = true
		if !hasValue {
			i++
			if i >= len(words) {
				return class, fmt.Errorf("%s requires a value", key)
			}
			value = words[i]
		}
		if key == "--username" {
			if !class.Login || value == "" {
				return class, errors.New("username applies only to recovery login")
			}
			continue
		}
		if class.Action != "add" {
			return class, errors.New("profile URL/name flags apply only to profile add")
		}
		if key == "--url" && value != controllerURL {
			return class, errors.New("recovery must use the installed controller URL")
		}
		if key == "--name" && value != profileName {
			return class, errors.New("recovery must use the managed profile name")
		}
	}
	if class.Action == "add" {
		if !seen["--name"] {
			class.Args = append(class.Args, "--name", profileName)
		}
		if !seen["--url"] {
			class.Args = append(class.Args, "--url", controllerURL)
		}
	}
	if (class.Action == "delete" || class.Action == "switch") && positionals != 1 {
		return class, errors.New("specify the managed garm-orbstack profile name")
	}
	return class, nil
}

// GarmCLI forwards ordinary commands only after controller-bound authentication.
// Recovery is deliberately possible with an expired token or absent CLI home.
func GarmCLI(ctx context.Context, args []string) error {
	if err := ValidateReleaseMetadata(); err != nil {
		return err
	}
	class, err := classifyCLI(args)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	p, err := pathsFor(home, "", releaseinfo.Version)
	if err != nil {
		return err
	}
	if err := p.check(); err != nil {
		return err
	}
	host, err := config.LoadHost(p.HostConfig)
	if err != nil {
		return err
	}
	record, err := loadInstallation(p)
	if err != nil {
		return err
	}
	if record.Version != releaseinfo.Version || record.ControllerID != host.ControllerID || host.StateDir != p.State {
		return errors.New("wrapper release/controller binding differs from the installed controller")
	}
	if _, err := currentRelease(ctx, p); err != nil {
		return err
	}
	if !class.Recovery {
		if _, err := readController(ctx, p, host.ControllerID); err != nil {
			return err
		}
	} else if class.Action != "add" && class.Action != "list" && class.Action != "delete" {
		if err := validateProfile(p, false); err != nil {
			return err
		}
	} else if class.Action == "add" {
		// A damaged or foreign profile must be removed explicitly, not silently
		// loaded by upstream initConfig before the login operation.
		if data, err := os.ReadFile(profilePath(p)); err == nil && len(bytes.TrimSpace(data)) != 0 {
			if err := validateProfile(p, false); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := cliInteractive(ctx, p, class.Args...); err != nil {
		return err
	}
	if class.Login {
		_, err := readController(ctx, p, host.ControllerID)
		return err
	}
	return nil
}
