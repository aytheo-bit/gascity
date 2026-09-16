package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/humantrust"
	"github.com/spf13/cobra"
)

// newMailTrustCmd returns "gc mail trust", the provisioning surface for the
// operator-only trust key that backs `gc mail send --sign` (see
// internal/humantrust and docs/reference/trust-boundaries.md). Every
// subcommand here is meant to be run directly by a human in their own
// terminal (or an explicitly provisioned trusted relay process) — never as a
// side effect of anything a spawned session does — and "init" refuses
// outright when this process carries a managed-session identity, mirroring
// refuseUnauthenticatedHumanSender's defense-in-depth style.
func newMailTrustCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Manage the operator-only key that signs verified-human/controller mail",
		Long: `Manage the Ed25519 keypair that backs "gc mail send --sign".

The private key lets its holder produce a signature that "gc mail check
--inject", "gc mail inbox", and any other mail reader can independently
confirm came from a genuine human operator (or an explicitly provisioned
trusted relay) claiming the reserved "human" or "controller" sender identity
— without that reader ever needing the private key itself.

Run "gc mail trust init" once, directly in your own terminal, before using
--sign. Never run it from inside a managed agent session.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc mail trust: missing subcommand (init, show)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc mail trust: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newMailTrustInitCmd(stdout, stderr),
		newMailTrustShowCmd(stdout, stderr),
	)
	return cmd
}

func newMailTrustInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate the operator-only signing keypair",
		Long: `Generate a new Ed25519 keypair for "gc mail send --sign".

The private key is written with mode 0600 to a machine-local, Gas-City-home
path (see internal/gchome) that ordinary session commands never read. The
public key is written alongside it, world-readable by design — sharing it
does not let anyone forge a signature.

Refuses to run when this process carries a managed-session identity
(GC_SESSION_ID, GC_ALIAS, or GC_AGENT set): this command must be run by a
human directly, or by an explicitly provisioned trusted relay process, never
by a spawned agent session. Refuses to overwrite an existing key unless
--force is given, since rotating the key invalidates every previously
verified message's ability to re-verify under the new key.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdMailTrustInit(force, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing key (invalidates previously verified mail)")
	return cmd
}

func cmdMailTrustInit(force bool, stdout, stderr io.Writer) int {
	if ambientManagedSession() {
		fmt.Fprintln(stderr, "gc mail trust init: refusing: this process has a managed session identity (GC_SESSION_ID, GC_ALIAS, or GC_AGENT is set); run this from a genuine human terminal") //nolint:errcheck // best-effort stderr
		return 1
	}
	pub, priv, err := humantrust.GenerateKeyPair()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail trust init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := humantrust.WriteKeyPair(pub, priv, force); err != nil {
		fmt.Fprintf(stderr, "gc mail trust init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Wrote signing key to %s\n", humantrust.PrivateKeyPath())               //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Wrote public key to %s\n", humantrust.PublicKeyPath())                 //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Public key fingerprint: %s\n", humantrust.Fingerprint(pub))            //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Keep the private key file secret. The public key is safe to share —") //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "copy it to any other host that needs to verify your signed mail.")    //nolint:errcheck // best-effort stdout
	return 0
}

func newMailTrustShowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the public key and fingerprint",
		Long: `Print the currently provisioned public key and its fingerprint.

Safe to run from anywhere, including inside a spawned session: it only
reveals the public key, which does not let its reader forge a signature.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdMailTrustShow(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func cmdMailTrustShow(stdout, stderr io.Writer) int {
	pub, err := humantrust.LoadPublicKey()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail trust show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Public key path: %s\n", humantrust.PublicKeyPath()) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Fingerprint: %s\n", humantrust.Fingerprint(pub))    //nolint:errcheck // best-effort stdout
	return 0
}
