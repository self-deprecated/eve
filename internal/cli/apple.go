package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/apple"
	"github.com/spf13/cobra"
)

// These run on the machine running eve, not the build server (docs/design.md §20).
func appleCmd(env *Env) *cobra.Command {
	var team string
	root := &cobra.Command{
		Use:   "apple",
		Short: "Create bundle ids, App Store Connect records and APNs keys from this machine (needs fastlane)",
		Long: "Provisions an app on Apple's side over an Apple ID web session cached on this machine by\n" +
			"`eve apple login`; the build server isn't involved. Exit codes: 0 done, 1 failed, 64 usage,\n" +
			"65 Apple refused, 69 Apple unreachable, 78 needs a human (sign in again: `eve apple login`).",
	}
	root.PersistentFlags().StringVar(&team, "team", os.Getenv("EVE_APPLE_TEAM"), "Apple team id (default $EVE_APPLE_TEAM, or the account's only team)")
	app := &cobra.Command{Use: "app", Short: "Provision apps"}
	app.AddCommand(appleAppCreateCmd(env, &team))
	key := &cobra.Command{Use: "key", Short: "Create APNs keys"}
	key.AddCommand(appleKeyCreateCmd(env, &team))
	root.AddCommand(appleLoginCmd(env, &team), appleAppsCmd(env, &team), appleKeysCmd(env, &team), app, key)
	return root
}

// appleFailure prints an Apple error (with --json, as a document carrying whatever was done
// before it) and returns the matching exit code.
func (e *Env) appleFailure(err error, partial any) error {
	var ae *apple.Error
	if !errors.As(err, &ae) {
		return err
	}
	if e.JSON {
		_ = e.printJSON(struct {
			Outcome api.State  `json:"outcome"`
			Error   string     `json:"error"`
			Human   *api.Human `json:"human,omitempty"`
			Result  any        `json:"result,omitempty"`
		}{ae.Outcome(), ae.Error(), ae.Human(), partial})
	}
	return &ExitError{Code: ae.ExitCode(), Err: ae}
}

func teamLabel(a apple.Account) string {
	if a.TeamName == "" {
		return a.Team
	}
	return fmt.Sprintf("%s (%s)", a.Team, a.TeamName)
}

func appleLoginCmd(env *Env, team *string) *cobra.Command {
	var appleID, passwordDoppler string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to Apple (password and two-factor code) and cache the session on this machine",
		Long: "Signs in unless the cached session is still valid. The password is asked for in the\n" +
			"terminal, or read with the Doppler CLI from --password-doppler <project>/<config>/<SECRET>\n" +
			"(default $EVE_APPLE_PASSWORD_DOPPLER); Apple's two-factor code is always asked for.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("eve apple login needs a terminal: it asks for the Apple ID password and a two-factor code")}
			}
			a := env.NewApple()
			if appleID == "" {
				appleID = a.AppleID()
			}
			if appleID == "" {
				_, _ = fmt.Fprint(env.Err, "Apple ID (email): ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				appleID = strings.TrimSpace(line)
			}
			if appleID == "" {
				return &ExitError{Code: api.ExitUsage, Err: errors.New("no Apple ID given")}
			}
			res, err := a.Login(cmd.Context(), appleID, *team, passwordDoppler)
			if err != nil {
				return env.appleFailure(err, nil)
			}
			if env.JSON {
				return env.printJSON(res)
			}
			verb := "already signed in"
			if res.SignedIn {
				verb = "signed in"
			}
			_, _ = fmt.Fprintf(env.Out, "%s to Apple as %s", verb, res.AppleID)
			switch {
			case res.Team != "":
				_, _ = fmt.Fprintf(env.Out, ", team %s\n", teamLabel(res.Account))
			case len(res.Teams) > 0:
				names := make([]string, len(res.Teams))
				for i, t := range res.Teams {
					names[i] = fmt.Sprintf("%s (%s)", t.ID, t.Name)
				}
				_, _ = fmt.Fprintf(env.Out, "; teams %s: pass --team or set EVE_APPLE_TEAM\n", strings.Join(names, ", "))
			default:
				_, _ = fmt.Fprintln(env.Out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appleID, "apple-id", "", "Apple ID email (default: the last one used, or asked)")
	cmd.Flags().StringVar(&passwordDoppler, "password-doppler", os.Getenv("EVE_APPLE_PASSWORD_DOPPLER"),
		"read the password from this Doppler <project>/<config>/<SECRET> instead of asking (default $EVE_APPLE_PASSWORD_DOPPLER)")
	return cmd
}

func appleAppsCmd(env *Env, team *string) *cobra.Command {
	return &cobra.Command{
		Use:   "apps",
		Short: "List the team's App Store Connect apps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := env.NewApple().Apps(cmd.Context(), *team)
			if err != nil {
				return env.appleFailure(err, nil)
			}
			if env.JSON {
				return env.printJSON(res)
			}
			for _, a := range res.Apps {
				_, _ = fmt.Fprintf(env.Out, "%s  %s  %s  sku=%s  %s\n", a.ID, a.BundleID, a.Name, a.SKU, a.Locale)
			}
			return nil
		},
	}
}

func appleKeysCmd(env *Env, team *string) *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "List the team's keys (APNs and others; never their material)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := env.NewApple().Keys(cmd.Context(), *team)
			if err != nil {
				return env.appleFailure(err, nil)
			}
			if env.JSON {
				return env.printJSON(res)
			}
			for _, k := range res.Keys {
				_, _ = fmt.Fprintf(env.Out, "%s  %s  [%s]\n", k.ID, k.Name, strings.Join(k.Services, ", "))
			}
			return nil
		},
	}
}

func appleAppCreateCmd(env *Env, team *string) *cobra.Command {
	var s apple.AppSpec
	cmd := &cobra.Command{
		Use:   "create <bundle-id> [--name NAME --sku SKU]",
		Short: "Create what an app needs on Apple's side; anything that exists is left alone",
		Long: "Creates, in this order and only what's missing: the app group (group.<bundle-id>), the bundle\n" +
			"id with its capabilities, the App Store Connect record, then each extension's bundle id\n" +
			"(<bundle-id>.<extension>), then the internal TestFlight --group (with access to every build)\n" +
			"and --tester in it. --like copies an existing app's capabilities (with settings) and\n" +
			"app-group use, and each same-suffix extension's. --name and --sku are only needed when the\n" +
			"App Store Connect record doesn't exist yet. Running it again changes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s.BundleID = args[0]
			res, err := env.NewApple().CreateApp(cmd.Context(), *team, s)
			if err != nil {
				// Only what changed before the error: "no changes" would read as success.
				if !env.JSON && len(res.Changes) > 0 {
					_, _ = fmt.Fprint(env.Out, formatAppResult(res))
				}
				return env.appleFailure(err, res)
			}
			if env.JSON {
				return env.printJSON(res)
			}
			_, err = fmt.Fprint(env.Out, formatAppResult(res))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&s.Name, "name", "", "the app's name in App Store Connect (required for a new record)")
	f.StringVar(&s.SKU, "sku", "", "App Store Connect SKU (required for a new record)")
	f.StringVar(&s.Locale, "locale", "en-US", "primary locale")
	f.StringVar(&s.Version, "version", "1.0", "first version")
	f.StringVar(&s.Like, "like", "", "copy capabilities and app-group use from this existing bundle id and its extensions")
	f.StringArrayVar(&s.Extensions, "extension", nil, "register <bundle-id>.<extension> too (repeatable)")
	f.StringArrayVar(&s.Capabilities, "capability", nil, "enable this capability on the app, e.g. PUSH_NOTIFICATIONS (repeatable)")
	f.StringArrayVar(&s.ExtensionCapabilities, "extension-capability", nil, "enable this capability on every extension (repeatable)")
	f.BoolVar(&s.AppGroup, "app-group", false, "create group.<bundle-id> and add the app and extensions to it")
	f.StringVar(&s.Group, "group", "", "create this internal TestFlight group, which gets every build (server.yml testflight.group)")
	f.StringVar(&s.Tester, "tester", "", "add this App Store Connect user to --group (server.yml testflight.tester)")
	f.BoolVar(&s.DryRun, "dry-run", false, "only report what would change")
	return cmd
}

func formatAppResult(r apple.AppResult) string {
	var b strings.Builder
	if r.Team == "" {
		return ""
	}
	record := ""
	if r.AppID != "" {
		record = ", app record " + r.AppID
	}
	switch {
	case len(r.Changes) == 0:
		fmt.Fprintf(&b, "no changes: %s already has everything on Apple's side (team %s%s)\n", r.BundleID, r.Team, record)
	case r.DryRun:
		fmt.Fprintf(&b, "would change for %s (team %s, dry run):\n", r.BundleID, r.Team)
	default:
		fmt.Fprintf(&b, "changed for %s (team %s%s):\n", r.BundleID, r.Team, record)
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "  + %s\n", c)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

func appleKeyCreateCmd(env *Env, team *string) *cobra.Command {
	var s apple.KeySpec
	cmd := &cobra.Command{
		Use:   "create --name NAME --env sandbox|production|all --scope team|topic",
		Short: "Create an APNs key and store it in Doppler (APNS_KEY_P8, APNS_KEY_ID, APPLE_TEAM_ID)",
		Long: "Stores the key with the Doppler CLI's login in every --doppler config, creating missing\n" +
			"projects; APNS_KEY_P8 is restricted. --doppler <project> means the configs --env implies\n" +
			"(sandbox: dev, production: prd, all: dev and prd); <project>/<config> names one. Values go on\n" +
			"stdin and are never printed. A team key serves every topic; a topic key only the --topic\n" +
			"bundle ids, in one environment. Apple serves the .p8 once: if storing fails, the error names\n" +
			"the 0600 file that holds it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := env.NewApple().CreateKey(cmd.Context(), *team, s)
			if err != nil {
				return env.appleFailure(err, res)
			}
			if env.JSON {
				return env.printJSON(res)
			}
			created := ""
			if len(res.CreatedProjects) > 0 {
				created = " (created project " + strings.Join(res.CreatedProjects, ", ") + ")"
			}
			_, err = fmt.Fprintf(env.Out, "created APNs key %s (%s, %s, %s scope) in team %s; Doppler %s has APNS_KEY_P8, APNS_KEY_ID, APPLE_TEAM_ID%s\n",
				res.KeyID, res.Name, res.Env, res.Scope, res.Team, strings.Join(res.Stored, ", "), created)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&s.Name, "name", "", "key name: letters, digits and spaces (required)")
	f.StringVar(&s.Env, "env", "", "sandbox, production or all (required)")
	f.StringVar(&s.Scope, "scope", "team", "team (every topic) or topic (only --topic)")
	f.StringArrayVar(&s.Topics, "topic", nil, "bundle id a topic key serves (repeatable)")
	f.StringArrayVar(&s.Doppler, "doppler", nil, "Doppler <project> (configs from --env) or <project>/<config> to store the key in (repeatable, required)")
	return cmd
}
