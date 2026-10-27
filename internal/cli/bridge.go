package cli

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/client"
	"github.com/spf13/cobra"
)

func bridgeCmd(env *Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "bridge",
		Short: "Reach the iPhone over Tailscale while it is on another Wi-Fi network (travel)",
		Long: "The build Mac normally reaches the phone only on its own network. `eve bridge on` lets it reach the\n" +
			"phone over Tailscale for a while instead: the phone needs Wi-Fi (any network, not mobile data)\n" +
			"with Tailscale connected, and must have been on the build Mac's Wi-Fi at least once before.\n" +
			"Turn it on when a device job reports connect_phone and the phone is away; it ends by itself.",
	}
	var f api.BridgeRequest
	on := &cobra.Command{
		Use:   "on",
		Short: "Start (or extend) the bridge and check the phone answers through it",
		Long: "Exit codes: 0 the phone answered through the bridge, 69 the bridge is on but the phone didn't\n" +
			"answer (check its Wi-Fi and Tailscale, then run it again), 1 the bridge couldn't start.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if d, err := time.ParseDuration(f.For); err != nil || d <= 0 {
				return &ExitError{Code: api.ExitUsage, Err: errors.New(`--for must be a positive duration, e.g. 2h`)}
			}
			c, err := env.client()
			if err != nil {
				return err
			}
			b, err := c.BridgeOn(cmd.Context(), f)
			if err != nil {
				return bridgeErr(err)
			}
			if err := env.writeBridge(b); err != nil {
				return err
			}
			if !b.Reachable {
				return &ExitError{Code: api.ExitUnavailable}
			}
			return nil
		},
	}
	on.Flags().StringVar(&f.For, "for", "2h", "how long from now (capped by the server's device.bridge.maxDuration)")
	on.Flags().StringVar(&f.Phone, "phone", "", "the phone's tailnet name or address (default: device.bridge.phone, else the only iPhone online)")
	off := &cobra.Command{
		Use: "off", Short: "Stop the bridge", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			b, err := c.BridgeOff(cmd.Context())
			if err != nil {
				return bridgeErr(err)
			}
			return env.writeBridge(b)
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Show whether the bridge is on, until when, and whether the phone answered", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			b, err := c.Bridge(cmd.Context())
			if err != nil {
				return bridgeErr(err)
			}
			return env.writeBridge(b)
		},
	}
	root.AddCommand(on, off, status)
	return root
}

// bridgeErr explains a server without the bridge.
func bridgeErr(err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed) {
		return &ExitError{Code: api.ExitUnavailable, Err: errors.New("the eve server is older than this client and has no bridge: update eve on the build Mac")}
	}
	return err
}

func (e *Env) writeBridge(b api.Bridge) error {
	if e.JSON {
		return e.printJSON(b)
	}
	_, err := fmt.Fprint(e.Out, FormatBridge(b))
	return err
}

// FormatBridge renders the bridge's state for a person.
func FormatBridge(b api.Bridge) string {
	var s string
	if b.On {
		answered := "the phone answered through it"
		if !b.Reachable {
			answered = "the phone hasn't answered through it yet"
		}
		s = fmt.Sprintf("bridge on until %s to %s; %s\n", b.Until.Local().Format("Mon 15:04"), b.Phone, answered)
	} else {
		s = "bridge off\n"
	}
	if b.Detail != "" && (!b.On || !b.Reachable) {
		s += b.Detail + "\n"
	}
	if !b.AdvertSeenAt.IsZero() {
		s += fmt.Sprintf("phone last seen on the build Mac's network %s\n", b.AdvertSeenAt.Local().Format("Mon 2 Jan 15:04"))
	}
	return s
}
