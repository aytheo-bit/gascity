package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/humantrust"
	"github.com/spf13/cobra"
)

// newMailTrustCmd returns "gc mail trust", the provisioning surface for the
// operator-only trust key that backs verified mail (see internal/humantrust
// and docs/reference/trust-boundaries.md).
//
// "init" and "sign" load or need the PRIVATE key and are meant to be run
// directly by a human in their own terminal — or an explicitly provisioned
// trusted relay — on a machine that does NOT run the gascity orchestrator or
// any spawned session. Both refuse outright when this process carries a
// managed-session identity, mirroring refuseUnauthenticatedHumanSender's
// defense-in-depth style, but that check is not what makes the key safe:
// what makes it safe is that the operator only ever runs these commands on
// a machine with no shared filesystem with any node that spawns sessions.
//
// "show" and "import" only ever handle the PUBLIC key, which means neither
// can ever leak or require the private key — "import" is in fact meant to
// be run on a node that spawns sessions, once per node, to provision that
// node's read-side verification. "Safe" here is scoped to confidentiality
// only, though: "import" has no managed-session gate and, by design, will
// happily overwrite the node's trusted key (with --force) for ANY caller,
// including a spawned session replacing it with a key of its own. That is
// an accepted, currently-undefended integrity gap — see internal/humantrust
// and docs/reference/trust-boundaries.md's "same-UID trust anchor
// replacement" material — not something this command's own "safe to run
// anywhere" framing should be read to rule out.
func newMailTrustCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Manage the operator-only key that signs verified-human/controller mail",
		Long: `Manage the Ed25519 keypair that backs verified "human"/"controller" mail.

The private key lets its holder produce a signature that "gc mail check
--inject", "gc mail inbox", and any other mail reader can independently
confirm came from a genuine human operator (or an explicitly provisioned
trusted relay) claiming the reserved "human" or "controller" sender identity
— without that reader ever needing the private key itself.

Run "gc mail trust init" once, on your OWN machine — never on a node that
runs the gascity orchestrator or any spawned session — before using "gc mail
trust sign" or "gc mail send --sign". Then run "gc mail trust import" on
every node that needs to verify your signed mail, including nodes that run
spawned sessions: it only ever writes the public key.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc mail trust: missing subcommand (init, show, sign, import)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc mail trust: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newMailTrustInitCmd(stdout, stderr),
		newMailTrustShowCmd(stdout, stderr),
		newMailTrustSignCmd(stdout, stderr),
		newMailTrustImportCmd(stdout, stderr),
	)
	return cmd
}

func newMailTrustInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate the operator-only signing keypair",
		Long: `Generate a new Ed25519 keypair for verified "human"/"controller" mail.

Run this ONLY on your own machine, or an explicitly provisioned trusted
relay machine — never on a node that runs the gascity orchestrator or any
spawned session. The private key is written with mode 0600 to a
machine-local, Gas-City-home path (see internal/gchome). Nothing about this
command's own execution enforces "the right machine": that is an operator
deployment decision this command cannot verify, only warn about.

The public key is written alongside it, world-readable by design. Copy it to
every node that needs to verify your signed mail with "gc mail trust show"
and "gc mail trust import" — never by copying the private key file itself.

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
	fmt.Fprintf(stdout, "Wrote signing key to %s\n", humantrust.PrivateKeyPath())                       //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Wrote public key to %s\n", humantrust.PublicKeyPath())                         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Public key fingerprint: %s\n", humantrust.Fingerprint(pub))                    //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Keep the private key file on THIS machine only. Never copy it to, or run")    //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "any 'gc mail trust' command that touches it from, a node that runs the")      //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "gascity orchestrator or any spawned session — that reintroduces the exact")   //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "gap this key design closes.")                                                 //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "")                                                                            //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "The public key is safe to share. Record the fingerprint above somewhere")     //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "durable now (password manager, team wiki) — you'll need it to confirm, out-") //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "of-band, that 'gc mail trust import' on every verifying node received the")   //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "real key and not a planted one. Run 'gc mail trust show' to print the")       //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "base64 public key to copy onto each verifying node.")                         //nolint:errcheck // best-effort stdout
	return 0
}

func newMailTrustShowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the public key and fingerprint",
		Long: `Print the currently provisioned public key and its fingerprint.

Safe to run from anywhere, including inside a spawned session: it only
reveals the public key, which does not let its reader forge a signature.

Copy the base64 public key this prints and pass it to "gc mail trust
import" on every node that needs to verify your signed mail. After
importing, compare the fingerprint "gc mail trust import" (or a follow-up
"gc mail trust show" on that node) reports against the fingerprint printed
here — over a channel other than the one the key blob traveled over — to
catch a pre-planted or substituted key.`,
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
	fmt.Fprintf(stdout, "Public key path: %s\n", humantrust.PublicKeyPath())                      //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Public key (base64): %s\n", humantrust.EncodePublicKeyString(pub))       //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Fingerprint: %s\n", humantrust.Fingerprint(pub))                         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "To let another node verify your signed mail, copy the base64 public")   //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "key above and run 'gc mail trust import <that value>' on that node,")   //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "then confirm the fingerprint it reports matches the one above through") //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "a separate, already-trusted channel before relying on it.")             //nolint:errcheck // best-effort stdout
	return 0
}

func newMailTrustSignCmd(stdout, stderr io.Writer) *cobra.Command {
	var from string
	var subject string
	var message string
	cmd := &cobra.Command{
		Use:   "sign <to> <body>",
		Short: "Sign a mail payload for later relay into a target node",
		Long: `Sign a mail payload without sending it, producing a portable envelope.

Run this ONLY on the machine that holds the private key — your own machine,
or an explicitly provisioned trusted relay — never on a node that runs the
gascity orchestrator or any spawned session (see "gc mail trust init"'s same
constraint; the private key is loaded here exactly as it is there).

<to> must be the exact address the TARGET node will resolve the recipient
to (typically a plain session alias, e.g. "mayor"): the signature covers the
identity, recipient, and body exactly as given, and the target node's
"gc mail send --signed-envelope" recomputes the same resolution before
verifying, so a recipient string that resolves differently there than it
would have here makes the relay fail closed with a clear error, not a
silent mismatch.

Prints ONLY the opaque envelope token to stdout (safe to capture into a
shell variable or pipe straight into an SSH command); explanatory text goes
to stderr. Relay it with:

  gc mail trust sign mayor "approve the deploy" | ssh node-c gc mail send --signed-envelope -

or capture it first:

  token=$(gc mail trust sign mayor "approve the deploy")
  ssh node-c gc mail send --signed-envelope "$token"

The target node never needs, loads, or receives the private key to accept
this — it re-verifies the signature using only the public key already
provisioned there via "gc mail trust import".

Refuses to run when this process carries a managed-session identity, and
refuses when no trust key is provisioned (run "gc mail trust init" first).
The envelope is only valid for internal/humantrust.MaxSignatureAge (15
minutes) from the moment this command runs — relay it promptly.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailTrustSign(args[0], args[1], from, subject, message, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "human", "reserved identity to sign as (\"human\" or \"controller\")")
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "message subject line (not covered by the signature)")
	cmd.Flags().StringVarP(&message, "message", "m", "", "message body text (overrides the positional <body> if both are given)")
	return cmd
}

func cmdMailTrustSign(to, body, from, subject, message string, stdout, stderr io.Writer) int {
	if from != "human" && from != "controller" {
		fmt.Fprintf(stderr, "gc mail trust sign: --from must be \"human\" or \"controller\" (got %q)\n", from) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ambientManagedSession() {
		fmt.Fprintln(stderr, "gc mail trust sign: refusing: this process has a managed session identity (GC_SESSION_ID, GC_ALIAS, or GC_AGENT is set); only a genuinely unmanaged human terminal or an explicitly provisioned trust relay may sign") //nolint:errcheck // best-effort stderr
		return 1
	}
	if message != "" {
		body = message
	}
	if strings.TrimSpace(to) == "" {
		fmt.Fprintln(stderr, "gc mail trust sign: recipient is required") //nolint:errcheck // best-effort stderr
		return 1
	}
	key, err := humantrust.LoadPrivateKey()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail trust sign: %v; run 'gc mail trust init' from a genuine human terminal first\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	issuedAt := time.Now()
	env := humantrust.BuildEnvelope(key, from, to, subject, body, issuedAt)
	token, err := humantrust.EncodeEnvelope(env)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail trust sign: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, token)                                                                                                                                                                                                        //nolint:errcheck // the token itself is the machine-usable output
	fmt.Fprintf(stderr, "Signed as %q for %q, valid for %s from %s. Relay with:\n  gc mail send --signed-envelope <token>\nrun against the target node.\n", from, to, humantrust.MaxSignatureAge, issuedAt.UTC().Format(time.RFC3339)) //nolint:errcheck // best-effort stderr
	return 0
}

func newMailTrustImportCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	var expectFingerprint string
	cmd := &cobra.Command{
		Use:   "import <base64-public-key>",
		Short: "Provision a verifying node with the operator's public key",
		Long: `Write ONLY a public key to this node's trust directory.

Run this on every node that needs to verify signed mail, including nodes
that run the gascity orchestrator and spawned sessions: unlike "init", this
command has no code path that creates, receives, writes, or otherwise needs
the private key, so running it here does not reintroduce the gap the key
redesign closes.

Paste the exact base64 value "gc mail trust show" printed on the machine
that holds the private key. Refuses to overwrite an existing public key
unless --force is given, since replacing a node's trusted key is a
trust-changing operation.

Pass --expect-fingerprint with the fingerprint you confirmed out-of-band
(over a channel other than the one the key blob traveled over) to make this
command itself refuse a mismatch, rather than relying only on a manual
comparison afterward. This does not eliminate the risk of a pre-planted key
if this node's filesystem was already compromised before you ran this
command — it only catches a key blob that was tampered with, or a wrong
key pasted, in this one step.

This command intentionally has NO managed-session gate (unlike "init" and
"sign"): it is meant to be safe to run from inside a spawned session,
because it can never leak or require the private key. That same lack of a
gate means a spawned session on this node can itself run "gc mail trust
import --force <any key>" and silently replace this node's trusted key with
one of its own choosing, with its completely ordinary session environment
intact — no elevated access or env-stripping needed. This is an accepted,
currently-undefended gap (see docs/reference/trust-boundaries.md's
"same-UID trust anchor replacement"), not something --force or
--expect-fingerprint close against an adversary with the node's own shell
access.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailTrustImport(args[0], force, expectFingerprint, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing trusted public key")
	cmd.Flags().StringVar(&expectFingerprint, "expect-fingerprint", "", "refuse unless the imported key's fingerprint matches this out-of-band-confirmed value")
	return cmd
}

func cmdMailTrustImport(blob string, force bool, expectFingerprint string, stdout, stderr io.Writer) int {
	pub, err := humantrust.ParsePublicKeyString(blob)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail trust import: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fingerprint := humantrust.Fingerprint(pub)
	if expectFingerprint != "" && !strings.EqualFold(strings.TrimSpace(expectFingerprint), fingerprint) {
		fmt.Fprintf(stderr, "gc mail trust import: fingerprint %s does not match --expect-fingerprint %s; refusing (confirm the correct key out-of-band before retrying)\n", fingerprint, expectFingerprint) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := humantrust.WritePublicKeyOnly(pub, force); err != nil {
		fmt.Fprintf(stderr, "gc mail trust import: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Wrote public key to %s\n", humantrust.PublicKeyPath()) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Fingerprint: %s\n", fingerprint)                       //nolint:errcheck // best-effort stdout
	if expectFingerprint == "" {
		fmt.Fprintln(stdout, "Confirm this fingerprint matches 'gc mail trust show' on the key-holding") //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "machine over a separate, already-trusted channel before relying on it.")   //nolint:errcheck // best-effort stdout
	}
	return 0
}
