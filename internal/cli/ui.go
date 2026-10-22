package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
)

func (a *App) uiCommand() *cobra.Command {
	var printURL bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web dashboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load("")
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			url, err := uiURL(cfg.UI.Listen, readToken, tailscaleIP)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			if printURL {
				fmt.Fprintln(a.Out, url)
				return nil
			}
			fmt.Fprintln(a.Out, url)
			if err := openBrowser(url); err != nil {
				fmt.Fprintln(a.Err, "kran: could not open a browser here; open the URL above.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&printURL, "print-url", false, "only print the URL (it includes the token; treat it like a password)")
	return cmd
}

func readToken() string {
	b, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func tailscaleIP() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Fields(string(out) + " ")[0])
}

// uiURL builds the dashboard URL from ui.listen (SPEC §9). It always
// carries the token in the fragment (never sent to the server or logged):
// the API needs it on loopback too.
func uiURL(listen string, token func() string, tsIP func() string) (string, error) {
	if listen == "" {
		return "", fmt.Errorf("the web UI is disabled; enable it by setting [ui] listen = \"127.0.0.1:7270\" in %s and restarting the daemon", paths.ConfigFile())
	}
	host, port := listen, "7270"
	if listen == "tailscale" {
		host = tsIP()
		if host == "" {
			return "", fmt.Errorf("ui.listen is \"tailscale\" but no tailscale address was found; start tailscale or set a host:port")
		}
	} else if h, p, err := net.SplitHostPort(listen); err == nil {
		host, port = h, p
	}
	u := "http://" + net.JoinHostPort(host, port) + "/"
	if t := token(); t != "" {
		u += "#token=" + t
	}
	return u, nil
}

func openBrowser(url string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	default:
		name = "xdg-open"
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("no display")
		}
	}
	return exec.Command(name, url).Start()
}
