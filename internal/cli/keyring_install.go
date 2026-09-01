package cli

// keyring_install.go — copy keyring files into place with the modes
// the keystore gates demand.
//
// Born from issue #46's Kubernetes reality: Secret and projected
// volumes are symlink farms whose file modes the pod's fsGroup
// rewrites (group-read gets OR'd in), and the keystore refuses any
// group/other bit on kek.bin or the signing key — correctly, that is
// the gate's whole point. The standard countermeasure is an
// initContainer that copies the mounted Secrets into an emptyDir with
// owner-only modes; our runtime image is distroless (no shell, no
// cp), so the agent carries the copy itself. The sidecar chart's
// initContainer runs exactly:
//
//	pg_hardstorage keyring install --from /keyring-src --to /etc/pg_hardstorage/keyring
//
// Copies whichever of the three canonical files exist at the source
// (any subset — a plaintext-only deployment has no kek.bin), follows
// the ..data symlinks Kubernetes creates, writes the private key and
// KEK as 0600 and the public key as 0644, and refuses an entirely
// empty source — a misconfigured mount must fail the init, not
// produce a pod that limps along keyless.

import (
	"fmt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func newKeyringCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "keyring",
		Short: "Keyring utilities",
	}
	c.AddCommand(newKeyringInstallCmd())
	return c
}

func newKeyringInstallCmd() *cobra.Command {
	var from, to string
	c := &cobra.Command{
		Use:   "install --from <dir> --to <dir>",
		Short: "Copy keyring files into place with the modes the keystore requires",
		Long: `install copies the canonical keyring files from one directory to
another, writing each with the permissions the keystore's gates
demand: 0600 for ` + keystore.PrivateKeyFile + ` and ` + keystore.KEKFileName + `,
0644 for ` + keystore.PublicKeyFile + `.

Built for Kubernetes initContainers: Secret and projected volumes are
symlink farms whose modes fsGroup rewrites (group-read is OR'd in),
and the keystore refuses group/other bits — correctly. Copying into
an emptyDir with this command yields files owned by the running UID
with owner-only modes, immune to fsGroup.

Any subset of the three files may exist at the source (plaintext-only
deployments carry no kek.bin). An entirely empty source is refused:
a misconfigured mount must fail the initContainer loudly, not produce
a pod that limps along keyless.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKeyringInstall(cmd, from, to)
		},
	}
	c.Flags().StringVar(&from, "from", "", "source directory (e.g. a mounted Secret) (required)")
	c.Flags().StringVar(&to, "to", "", "destination keyring directory (required)")
	_ = c.MarkFlagRequired("from")
	_ = c.MarkFlagRequired("to")
	return c
}

// keyringInstallFiles maps each canonical file to the mode its gate
// demands. Filenames come from the keystore's own constants so this
// command cannot drift from what the agent later opens.
var keyringInstallFiles = []struct {
	Name string
	Mode os.FileMode
}{
	{keystore.PrivateKeyFile, 0o600},
	{keystore.KEKFileName, 0o600},
	{keystore.PublicKeyFile, 0o644},
}

func runKeyringInstall(cmd *cobra.Command, from, to string) error {
	d := DispatcherFrom(cmd)
	if err := os.MkdirAll(to, 0o700); err != nil {
		return output.NewError("keyring.install_failed",
			fmt.Sprintf("keyring install: create %q: %v", to, err)).Wrap(err)
	}
	var installed []string
	for _, f := range keyringInstallFiles {
		src := filepath.Join(from, f.Name)
		// os.Open follows symlinks — Kubernetes serves Secret files
		// through the ..data indirection.
		in, err := os.Open(src)
		if os.IsNotExist(err) {
			continue // any subset is legitimate
		}
		if err != nil {
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: open %q: %v", src, err)).Wrap(err)
		}
		dst := filepath.Join(to, f.Name)
		tmp := dst + ".tmp"
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode)
		if err != nil {
			_ = in.Close()
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: create %q: %v", tmp, err)).Wrap(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = in.Close()
			_ = out.Close()
			_ = os.Remove(tmp)
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: copy %s: %v", f.Name, err)).Wrap(err)
		}
		_ = in.Close()
		// fsync before the rename. io.Copy + Close pushes the bytes no
		// further than the page cache, so a power loss can land the
		// rename with a zero-length or partial key file behind it —
		// and a truncated private key is worse than a missing one:
		// LoadOrGenerate sees both halves present, fails to parse, and
		// refuses to start rather than regenerating.
		if err := out.Sync(); err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: fsync %q: %v", tmp, err)).Wrap(err)
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmp)
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: close %q: %v", tmp, err)).Wrap(err)
		}
		// O_CREATE honours umask; the gate wants the EXACT owner-only
		// set, so pin it after the write.
		if err := os.Chmod(tmp, f.Mode); err != nil {
			_ = os.Remove(tmp)
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: chmod %q: %v", tmp, err)).Wrap(err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: rename to %q: %v", dst, err)).Wrap(err)
		}
		installed = append(installed, f.Name)
	}
	// And the parent dentry list once, after the loop: file fsync does
	// not cover the renames. Without it the installed keyring can
	// vanish on a power loss even though every file was synced.
	if len(installed) > 0 {
		if err := fsutil.SyncDir(to); err != nil {
			return output.NewError("keyring.install_failed",
				fmt.Sprintf("keyring install: fsync %q: %v", to, err)).Wrap(err)
		}
	}
	if len(installed) == 0 {
		return output.NewError("keyring.install_empty_source",
			fmt.Sprintf("keyring install: none of the keyring files (%s, %s, %s) exist under %q — refusing to leave the destination keyless",
				keystore.PrivateKeyFile, keystore.KEKFileName, keystore.PublicKeyFile, from)).
			WithSuggestion(&output.Suggestion{
				Human: "the source mount is empty or misconfigured. In Kubernetes, check the Secret names and keys the chart's keyring.* values reference, and that the Secrets exist in the release namespace.",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(keyringInstallBody{
		From: from, To: to, Installed: installed,
	}))
}

type keyringInstallBody struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Installed []string `json:"installed"`
}

// WriteText renders the install result as human-readable text to w.
func (b keyringInstallBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ installed %d keyring file(s) from %s to %s: %v",
		len(b.Installed), b.From, b.To, b.Installed)
	return err
}
