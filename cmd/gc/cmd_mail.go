package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/humantrust"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// nudgeFunc is an optional callback for nudging an agent after sending or
// replying to mail. When non-nil, it is called with the recipient name and
// the ID of the message the nudge announces. messageID lets the queued nudge
// carry a re-checkable reference, so delivery-time re-validation can withdraw
// it if the message is read or gone by then (gastownhall/gascity#5321).
// Errors are non-fatal.
type nudgeFunc func(recipient, messageID string) error

const (
	mailInjectMaxMessages          = 3
	mailInjectBodyPreviewSize      = 240
	mailInjectPreviewScanSize      = 4096
	mailCheckDegradedNotice        = "[mail check degraded — store slow; run 'gc mail inbox' when the factory load drops]"
	mailCheckPartialDegradedNotice = "[mail check degraded — partial provider read; run 'gc mail inbox' after the provider recovers]"
)

type mailInboxJSONResult struct {
	SchemaVersion string         `json:"schema_version"`
	Recipient     string         `json:"recipient"`
	Recipients    []string       `json:"recipients"`
	Messages      []mail.Message `json:"messages"`
}

type mailThreadJSONResult struct {
	SchemaVersion string         `json:"schema_version"`
	ThreadID      string         `json:"thread_id"`
	Messages      []mail.Message `json:"messages"`
}

type mailMessageJSONResult struct {
	SchemaVersion string       `json:"schema_version"`
	Message       mail.Message `json:"message"`
}

type mailCountJSONResult struct {
	SchemaVersion string   `json:"schema_version"`
	Recipient     string   `json:"recipient"`
	Recipients    []string `json:"recipients"`
	Total         int      `json:"total"`
	Unread        int      `json:"unread"`
}

type mailActionResult struct {
	SchemaVersion string               `json:"schema_version"`
	OK            bool                 `json:"ok"`
	Command       string               `json:"command"`
	Action        string               `json:"action"`
	ID            string               `json:"id,omitempty"`
	Message       *mailMessageSummary  `json:"message,omitempty"`
	Messages      []mailMessageSummary `json:"messages,omitempty"`
	IDs           []string             `json:"ids,omitempty"`
	Count         *int                 `json:"count,omitempty"`
	AlreadyDone   bool                 `json:"already_done,omitempty"`
	Notified      bool                 `json:"notified,omitempty"`
	DryRun        bool                 `json:"dry_run,omitempty"`
}

type mailMessageSummary struct {
	ID               string `json:"id"`
	From             string `json:"from,omitempty"`
	To               string `json:"to,omitempty"`
	Subject          string `json:"subject,omitempty"`
	ThreadID         string `json:"thread_id,omitempty"`
	ReplyTo          string `json:"reply_to,omitempty"`
	Verified         bool   `json:"verified,omitempty"`
	VerifiedIdentity string `json:"verified_identity,omitempty"`
}

type mailArchiveSelectOptions struct {
	Recipient       string
	AllRecipients   bool
	From            string
	SubjectPrefix   string
	SubjectContains string
	EmptyBody       bool
	Limit           int
	IncludeRead     bool
	DryRun          bool
	CaseInsensitive bool
}

func summarizeMailMessage(m mail.Message) mailMessageSummary {
	return mailMessageSummary{
		ID:               m.ID,
		From:             m.From,
		To:               m.To,
		Subject:          m.Subject,
		ThreadID:         m.ThreadID,
		ReplyTo:          m.ReplyTo,
		Verified:         m.Verified,
		VerifiedIdentity: m.VerifiedIdentity,
	}
}

func newMailNudgeFunc(sender string) nudgeFunc {
	return func(recipient, messageID string) error {
		target, err := resolveNudgeTarget(recipient, io.Discard)
		if err != nil {
			return err
		}
		return sendMailNotify(target, sender, messageID)
	}
}

func newMailCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mail",
		Short: "Send and receive messages between agents and humans",
		Long: `Send and receive messages between agents and humans.

Mail is implemented as beads with type="message". Messages have a
sender, recipient, subject, and body. Use "gc mail check --inject" in agent
hooks to deliver mail notifications into agent prompts.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc mail: missing subcommand (archive, check, count, delete, inbox, mark-read, mark-unread, peek, read, reply, send, thread, trust)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc mail: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newMailArchiveCmd(stdout, stderr),
		newMailCheckCmd(stdout, stderr),
		newMailCountCmd(stdout, stderr),
		newMailDeleteCmd(stdout, stderr),
		newMailSendCmd(stdout, stderr),
		newMailInboxCmd(stdout, stderr),
		newMailMarkReadCmd(stdout, stderr),
		newMailMarkUnreadCmd(stdout, stderr),
		newMailPeekCmd(stdout, stderr),
		newMailReadCmd(stdout, stderr),
		newMailReplyCmd(stdout, stderr),
		newMailThreadCmd(stdout, stderr),
		newMailTrustCmd(stdout, stderr),
	)
	return cmd
}

func newMailArchiveCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	opts := mailArchiveSelectOptions{Limit: 100, CaseInsensitive: true}
	cmd := &cobra.Command{
		Use:   "archive <id>...",
		Short: "Archive one or more messages without reading them",
		Long: `Remove one or more message beads without displaying their contents.

Use this to dismiss messages without reading them. Each message is removed
and will no longer appear in mail check or inbox results. When multiple IDs
are passed, they are archived in input order.

For large advisory backlogs, use --to or --all-recipients with
--subject-prefix, --subject-contains, or --from to archive a bounded matching
slice without enumerating IDs by hand.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			switch {
			case opts.hasSelector():
				if jsonOut {
					code = cmdMailArchiveSelectedJSON(args, opts, true, stdout, stderr)
				} else {
					code = cmdMailArchiveSelectedJSON(args, opts, false, stdout, stderr)
				}
			case jsonOut:
				code = cmdMailArchiveJSON(args, true, stdout, stderr)
			default:
				code = cmdMailArchive(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	cmd.Flags().StringVar(&opts.Recipient, "to", "", "archive matching unread messages addressed to this recipient")
	cmd.Flags().BoolVar(&opts.AllRecipients, "all-recipients", false, "archive matching messages across all recipients")
	cmd.Flags().StringVar(&opts.From, "from", "", "archive matching unread messages from this exact sender")
	cmd.Flags().StringVar(&opts.SubjectPrefix, "subject-prefix", "", "archive matching unread messages whose subject starts with this text")
	cmd.Flags().StringVar(&opts.SubjectContains, "subject-contains", "", "archive matching unread messages whose subject contains this text")
	cmd.Flags().BoolVar(&opts.EmptyBody, "empty-body", false, "only archive matching messages whose body is empty")
	cmd.Flags().IntVar(&opts.Limit, "limit", opts.Limit, "maximum matching messages to archive in this run")
	cmd.Flags().BoolVar(&opts.IncludeRead, "include-read", false, "include read-but-open messages when selecting by filter")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "list matching messages without archiving them")
	return cmd
}

// cmdMailArchive is the CLI entry point for archiving a message.
func cmdMailArchive(args []string, stdout, stderr io.Writer) int {
	return cmdMailArchiveJSON(args, false, stdout, stderr)
}

func cmdMailArchiveJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail archive")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailArchiveJSON(mp, rec, args, jsonOut, stdout, stderr)
}

func (o mailArchiveSelectOptions) hasSelector() bool {
	return strings.TrimSpace(o.Recipient) != "" ||
		o.AllRecipients ||
		strings.TrimSpace(o.From) != "" ||
		strings.TrimSpace(o.SubjectPrefix) != "" ||
		strings.TrimSpace(o.SubjectContains) != "" ||
		o.EmptyBody ||
		o.IncludeRead ||
		o.DryRun
}

func (o mailArchiveSelectOptions) hasContentFilter() bool {
	return strings.TrimSpace(o.From) != "" ||
		strings.TrimSpace(o.SubjectPrefix) != "" ||
		strings.TrimSpace(o.SubjectContains) != ""
}

func cmdMailArchiveSelectedJSON(args []string, opts mailArchiveSelectOptions, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail archive")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailArchiveSelectedJSON(mp, rec, args, opts, jsonOut, stdout, stderr)
}

// doMailArchive archives one or more message beads. For a single ID the
// behavior matches the pre-batch CLI byte-for-byte; for two or more IDs it
// delegates to mp.ArchiveMany and prints one result line per id.
func doMailArchive(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doMailArchiveJSON(mp, rec, args, false, stdout, stderr)
}

func doMailArchiveSelected(mp mail.Provider, rec events.Recorder, opts mailArchiveSelectOptions, stdout, stderr io.Writer) int {
	return doMailArchiveSelectedJSON(mp, rec, nil, opts, false, stdout, stderr)
}

type archiveMatchingProvider interface {
	ArchiveCandidates(beadmail.ArchiveFilter) ([]mail.Message, error)
	ArchiveMatching(beadmail.ArchiveFilter) ([]mail.Message, []mail.ArchiveResult, error)
}

func doMailArchiveSelectedJSON(mp mail.Provider, rec events.Recorder, args []string, opts mailArchiveSelectOptions, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "gc mail archive: message IDs cannot be combined with --to/--from/--subject filters") //nolint:errcheck // best-effort stderr
		return 1
	}
	opts.Recipient = strings.TrimSpace(opts.Recipient)
	if opts.Recipient != "" && opts.AllRecipients {
		fmt.Fprintln(stderr, "gc mail archive: choose either --to or --all-recipients") //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.Recipient == "" && !opts.AllRecipients {
		fmt.Fprintln(stderr, "gc mail archive: --to or --all-recipients is required when using archive filters") //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.hasContentFilter() {
		fmt.Fprintln(stderr, "gc mail archive: use --from, --subject-prefix, or --subject-contains to avoid archiving unrelated mail") //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.Limit <= 0 {
		fmt.Fprintln(stderr, "gc mail archive: --limit must be greater than zero") //nolint:errcheck // best-effort stderr
		return 1
	}
	archiver, ok := mp.(archiveMatchingProvider)
	if !ok {
		fmt.Fprintln(stderr, "gc mail archive: filtered archive requires the beadmail provider") //nolint:errcheck // best-effort stderr
		return 1
	}
	recipients := []string(nil)
	if !opts.AllRecipients {
		recipients = []string{opts.Recipient}
	}
	filter := beadmail.ArchiveFilter{
		Recipients:      recipients,
		From:            opts.From,
		SubjectPrefix:   opts.SubjectPrefix,
		SubjectContains: opts.SubjectContains,
		EmptyBody:       opts.EmptyBody,
		IncludeRead:     opts.IncludeRead,
		CaseInsensitive: opts.CaseInsensitive,
		Limit:           opts.Limit,
	}
	if opts.DryRun {
		matches, err := archiver.ArchiveCandidates(filter)
		if err != nil {
			telemetry.RecordMailOp(context.Background(), "archive", err)
			fmt.Fprintf(stderr, "gc mail archive: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return renderMailArchiveSelection(matches, nil, opts, jsonOut, stdout, stderr)
	}
	matches, results, err := archiver.ArchiveMatching(filter)
	if err != nil {
		telemetry.RecordMailOp(context.Background(), "archive", err)
		fmt.Fprintf(stderr, "gc mail archive: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	exit := 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			telemetry.RecordMailOp(context.Background(), "archive", nil)
			rec.Record(events.Event{
				Type:    events.MailArchived,
				Actor:   eventActor(),
				Subject: r.ID,
				Payload: mailEventPayload(nil),
			})
		case errors.Is(r.Err, mail.ErrAlreadyArchived):
			// Candidate selection returns open messages, but preserve the
			// idempotent batch contract if a concurrent archive wins the race.
		default:
			telemetry.RecordMailOp(context.Background(), "archive", r.Err)
			fmt.Fprintf(stderr, "gc mail archive %s: %v\n", r.ID, r.Err) //nolint:errcheck // best-effort stderr
			exit = 1
		}
	}
	if jsonOut && exit != 0 {
		return exit
	}
	if renderExit := renderMailArchiveSelection(matches, results, opts, jsonOut, stdout, stderr); renderExit != 0 && exit == 0 {
		exit = renderExit
	}
	return exit
}

// splitMessageIDArgs splits every argument on whitespace and drops empty
// tokens. Message IDs never contain whitespace, and some shells can preserve a
// variable containing multiple IDs as one argument.
func splitMessageIDArgs(args []string) []string {
	ids := make([]string, 0, len(args))
	for _, arg := range args {
		ids = append(ids, strings.Fields(arg)...)
	}
	return ids
}

func doMailArchiveJSON(mp mail.Provider, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	args = splitMessageIDArgs(args)
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail archive: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(args) == 1 {
		if jsonOut {
			return doMailArchiveSingleJSON(mp, rec, args[0], true, stdout, stderr)
		}
		return doMailArchiveSingle(mp, rec, args[0], stdout, stderr)
	}
	if jsonOut {
		return doMailArchiveManyJSON(mp, rec, args, true, stdout, stderr)
	}
	return doMailArchiveMany(mp, rec, args, stdout, stderr)
}

func doMailArchiveSingle(mp mail.Provider, rec events.Recorder, id string, stdout, stderr io.Writer) int {
	return doMailArchiveSingleJSON(mp, rec, id, false, stdout, stderr)
}

func doMailArchiveSingleJSON(mp mail.Provider, rec events.Recorder, id string, jsonOut bool, stdout, stderr io.Writer) int {
	if err := mp.Archive(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			if jsonOut {
				return writeCLIJSONLineOrExit(stdout, stderr, "gc mail archive", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.archive", Action: "archive", ID: id, IDs: []string{id}, Count: intRef(0), AlreadyDone: true})
			}
			fmt.Fprintf(stdout, "Already archived %s\n", id) //nolint:errcheck // best-effort stdout
			return 0
		}
		telemetry.RecordMailOp(context.Background(), "archive", err)
		fmt.Fprintf(stderr, "gc mail archive: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "archive", nil)
	rec.Record(events.Event{
		Type:    events.MailArchived,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail archive", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.archive", Action: "archive", ID: id, IDs: []string{id}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Archived message %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

func doMailArchiveMany(mp mail.Provider, rec events.Recorder, ids []string, stdout, stderr io.Writer) int {
	return doMailArchiveManyJSON(mp, rec, ids, false, stdout, stderr)
}

func doMailArchiveManyJSON(mp mail.Provider, rec events.Recorder, ids []string, jsonOut bool, stdout, stderr io.Writer) int {
	results, err := mp.ArchiveMany(ids)
	if err != nil {
		telemetry.RecordMailOp(context.Background(), "archive", err)
		fmt.Fprintf(stderr, "gc mail archive: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	exit := 0
	archived := 0
	already := 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			archived++
			telemetry.RecordMailOp(context.Background(), "archive", nil)
			rec.Record(events.Event{
				Type:    events.MailArchived,
				Actor:   eventActor(),
				Subject: r.ID,
				Payload: mailEventPayload(nil),
			})
			if !jsonOut {
				fmt.Fprintf(stdout, "Archived message %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		case errors.Is(r.Err, mail.ErrAlreadyArchived):
			already++
			if !jsonOut {
				fmt.Fprintf(stdout, "Already archived %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		default:
			telemetry.RecordMailOp(context.Background(), "archive", r.Err)
			fmt.Fprintf(stderr, "gc mail archive %s: %v\n", r.ID, r.Err) //nolint:errcheck // best-effort stderr
			exit = 1
		}
	}
	if jsonOut && exit == 0 {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail archive", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.archive", Action: "archive", IDs: ids, Count: intRef(archived), AlreadyDone: already == len(ids)})
	}
	return exit
}

func renderMailArchiveSelection(matches []mail.Message, results []mail.ArchiveResult, opts mailArchiveSelectOptions, jsonOut bool, stdout, stderr io.Writer) int {
	ids := make([]string, 0, len(matches))
	summaries := make([]mailMessageSummary, 0, len(matches))
	for _, msg := range matches {
		ids = append(ids, msg.ID)
		summaries = append(summaries, summarizeMailMessage(msg))
	}
	if opts.DryRun {
		if jsonOut {
			return writeCLIJSONLineOrExit(stdout, stderr, "gc mail archive", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.archive", Action: "archive", IDs: ids, Messages: summaries, Count: intRef(len(matches)), DryRun: true})
		}
		if len(matches) == 0 {
			fmt.Fprintln(stdout, "No matching messages") //nolint:errcheck // best-effort stdout
			return 0
		}
		for _, msg := range matches {
			fmt.Fprintf(stdout, "Would archive message %s\t%s\n", msg.ID, msg.Subject) //nolint:errcheck // best-effort stdout
		}
		return 0
	}
	archived := 0
	already := 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			archived++
			if !jsonOut {
				fmt.Fprintf(stdout, "Archived message %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		case errors.Is(r.Err, mail.ErrAlreadyArchived):
			already++
			if !jsonOut {
				fmt.Fprintf(stdout, "Already archived %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		}
	}
	if len(matches) == 0 && !jsonOut {
		fmt.Fprintln(stdout, "No matching messages") //nolint:errcheck // best-effort stdout
	}
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail archive", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.archive", Action: "archive", IDs: ids, Messages: summaries, Count: intRef(archived), AlreadyDone: already == len(results) && len(results) > 0})
	}
	return 0
}

func newMailCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "check [session]",
		Short: "Check for unread mail (use --inject for hook output)",
		Long: `Check for unread mail addressed to a session alias or mailbox.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a <system-reminder> block suitable for
hook injection (always exits 0). The recipient defaults to $GC_SESSION_ID,
$GC_ALIAS, $GC_AGENT, or "human".`,
		Example: `  gc mail check
  gc mail check --inject
  gc mail check mayor`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailCheckWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

func cmdMailCheckWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	// --inject writes a <system-reminder> straight into a provider's system
	// prompt. With no recipient argument the mailbox falls back through
	// GC_SESSION_ID/GC_ALIAS/GC_AGENT to "human"
	// (defaultMailIdentityCandidates), so an unmanaged session — a human who
	// opened a provider in a directory gc staged overlays into — would have the
	// operator's own inbox injected as an instruction and act on it instead of
	// answering the human. Naming a mailbox is a deliberate request and is
	// still served; the plain non-inject form is untouched (#5304).
	if inject && len(args) == 0 && !hookHasManagedIdentity() {
		return 0
	}
	cityPath, cityPathErr := resolveCity()
	if cityPathErr == nil {
		if cfg, err := loadCityConfig(cityPath, stderr); err == nil && citySuspended(cfg) {
			if inject {
				return 0
			}
			fmt.Fprintln(stderr, "gc mail check: city is suspended") //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if cityPathErr != nil {
		return doMailCheckFallback(args, inject, hookFormat, stdout, stderr)
	}
	c, reason := mailCheckAPIClient(cityPath)
	return routeMailCheck(cityPath, args, inject, hookFormat, c, reason, stdout, stderr)
}

// mailCheckAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var mailCheckAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeMailCheck dispatches non-injecting `mail check` to the supervisor API
// when a controller is up; otherwise falls back to the local mail-provider path.
// Injecting hooks probe the API for degraded-read notices, then use the local
// path because provider-backed mail may need to perform delivery side effects
// after successful injection.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeMailCheck(_ string, args []string, inject bool, hookFormat string, c *api.Client, nilReason string, stdout, stderr io.Writer) int {
	const cmdName = "mail check"
	recipient := defaultMailIdentity()
	if len(args) > 0 {
		recipient = strings.TrimSpace(args[0])
	}
	if inject {
		if c != nil {
			cr, err := c.ListMailInbox(recipient, "")
			if err == nil {
				if mailListHasPartial(cr.Body) {
					logRoute(stderr, cmdName, "api", "error")
					notice := formatMailCheckPartialDegradedNotice()
					if mailListHasStoreSlowPartial(cr.Body) {
						notice = formatMailCheckDegradedNotice()
					}
					_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", notice)
					return 0
				}
			} else if !api.ShouldFallbackForRead(c, err) {
				logRoute(stderr, cmdName, "api", "error")
				if api.IsStoreSlowError(err) {
					_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", formatMailCheckDegradedNotice())
				}
				return 0
			}
		}
		logRoute(stderr, cmdName, "fallback", "inject-local-side-effects")
		return doMailCheckFallback(args, inject, hookFormat, stdout, stderr)
	}
	if c != nil {
		cr, err := c.ListMailInbox(recipient, "")
		if err == nil {
			if mailListHasPartial(cr.Body) {
				logRoute(stderr, cmdName, "api", "error")
				fmt.Fprintf(stderr, "gc mail check: %s\n", mailListPartialErrorDetail(cr.Body)) //nolint:errcheck // best-effort stderr
				return 1
			}
			logRoute(stderr, cmdName, "api", "")
			return renderMailCheckFromAPI(cr, recipient, inject, hookFormat, stdout)
		}
		if !api.ShouldFallbackForRead(c, err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc mail check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doMailCheckFallback(args, inject, hookFormat, stdout, stderr)
}

// renderMailCheckFromAPI formats the API-sourced inbox for `gc mail check`.
// With --inject, writes the <system-reminder> block and always returns 0.
// Without --inject, returns 0 if mail exists and 1 if empty, matching the
// local fallback contract; human output appends a stale-read banner when the
// supervisor cache is > 30 s old.
func renderMailCheckFromAPI(cr api.CachedRead[api.MailListView], recipient string, inject bool, hookFormat string, stdout io.Writer) int {
	messages := cr.Body.Items
	if inject {
		if len(messages) > 0 {
			_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", formatInjectOutput(messages))
		}
		return 0
	}
	if len(messages) == 0 {
		return 1
	}
	fmt.Fprintf(stdout, "%d unread message(s) for %s\n", len(messages), recipient) //nolint:errcheck // best-effort stdout
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func mailListHasStoreSlowPartial(view api.MailListView) bool {
	return mailPartialHasStoreSlow(view.Partial, view.PartialErrors)
}

func mailListHasPartial(view api.MailListView) bool {
	return view.Partial || len(view.PartialErrors) > 0
}

func mailListPartialErrorDetail(view api.MailListView) string {
	return mailPartialErrorDetail(view.PartialErrors, "partial mail read failed")
}

func mailCountHasPartial(view api.MailCountView) bool {
	return view.Partial || len(view.PartialErrors) > 0
}

func mailCountPartialErrorDetail(view api.MailCountView) string {
	return mailPartialErrorDetail(view.PartialErrors, "partial mail count failed")
}

func mailPartialHasStoreSlow(partial bool, partialErrors []string) bool {
	if !partial {
		return false
	}
	for _, msg := range partialErrors {
		if strings.Contains(msg, api.StoreSlowErrorCode+":") || strings.HasPrefix(msg, api.StoreSlowErrorCode) {
			return true
		}
	}
	return false
}

func mailPartialErrorDetail(partialErrors []string, fallback string) string {
	if len(partialErrors) == 0 {
		return fallback
	}
	return strings.Join(partialErrors, "; ")
}

func formatMailCheckDegradedNotice() string {
	return "<system-reminder>\n" + mailCheckDegradedNotice + "\n</system-reminder>\n"
}

func formatMailCheckPartialDegradedNotice() string {
	return "<system-reminder>\n" + mailCheckPartialDegradedNotice + "\n</system-reminder>\n"
}

// doMailCheckFallback is the direct-bd path for `gc mail check`.
func doMailCheckFallback(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail check")
	if mp == nil {
		if inject {
			return 0 // --inject always exits 0
		}
		return code
	}

	target, ok := resolveMailTargetFromArgs(args, stderr, "gc mail check")
	if !ok {
		if inject {
			return 0
		}
		return 1
	}

	return doMailCheckTargetWithFormat(mp, target, inject, hookFormat, stdout, stderr)
}

// doMailCheck checks for unread messages. Without --inject, prints the count
// and returns 0 if mail exists, 1 if empty. With --inject, outputs a
// <system-reminder> block for hook injection and always returns 0.
func doMailCheck(mp mail.Provider, recipient string, inject bool, stdout, stderr io.Writer) int {
	return doMailCheckTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, inject, stdout, stderr)
}

func doMailCheckTarget(mp mail.Provider, target resolvedMailTarget, inject bool, stdout, stderr io.Writer) int {
	return doMailCheckTargetWithFormat(mp, target, inject, "", stdout, stderr)
}

func doMailCheckTargetWithFormat(mp mail.Provider, target resolvedMailTarget, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	messages, err := collectMailMessages(mp.Check, target.recipients)
	if err != nil {
		if inject {
			fmt.Fprintf(stderr, "gc mail check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 0                                        // --inject always exits 0
		}
		fmt.Fprintf(stderr, "gc mail check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if inject {
		if len(messages) > 0 {
			if err := writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", formatInjectOutput(messages)); err != nil {
				fmt.Fprintf(stderr, "gc mail check: writing hook output: %v\n", err) //nolint:errcheck // best-effort stderr
				return 0
			}
			// Archive the SAME messages that were injected: priority-sort
			// before the clamp so the archived set matches formatInjectOutput's
			// displayed set (a priority:1 handoff that floats into the window is
			// injected AND archived, never injected-but-not-archived).
			injectedMessages := sortMailByPriority(messages)
			if len(injectedMessages) > mailInjectMaxMessages {
				injectedMessages = injectedMessages[:mailInjectMaxMessages]
			}
			archiveInjectedAutoHandoffMessages(mp, injectedMessages, stderr)
		}
		return 0 // --inject always exits 0
	}

	// Non-inject mode: print count, return 0 if mail, 1 if empty.
	if len(messages) == 0 {
		return 1
	}
	fmt.Fprintf(stdout, "%d unread message(s) for %s\n", len(messages), target.display) //nolint:errcheck // best-effort stdout
	return 0
}

type injectedAutoHandoffArchiver interface {
	ArchiveInjectedAutoHandoffs([]string) error
}

func archiveInjectedAutoHandoffMessages(mp mail.Provider, messages []mail.Message, stderr io.Writer) {
	archiver, ok := mp.(injectedAutoHandoffArchiver)
	if !ok {
		return
	}
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		ids = append(ids, msg.ID)
	}
	if err := archiver.ArchiveInjectedAutoHandoffs(ids); err != nil {
		fmt.Fprintf(stderr, "gc mail check: archiving injected auto handoff mail: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// sortMailByPriority returns a copy of messages ordered by descending Priority
// (higher first), stable so ties keep arrival (oldest-first) order. This runs
// BEFORE the mailInjectMaxMessages clamp so a higher-priority unread message
// (e.g. a restart handoff tagged priority:1) surfaces within the injection
// window instead of being dropped by arrival order. It returns a copy so the
// caller's (possibly cached, e.g. api.CachedRead) backing slice is never
// reordered as a side effect.
//
// Safety: mail.Message.Priority has no writer for ordinary mail today —
// extractPriority parses a numeric `priority:N` label and every normal message
// is priority 0 — so on existing all-priority-0 mail SliceStable is a provable
// no-op that preserves oldest-first order. Only newly priority-tagged mail
// floats. See STAGED-mail-priority (gastownhall/gascity Phase-4
// priority-stratified inbox check).
func sortMailByPriority(messages []mail.Message) []mail.Message {
	sorted := make([]mail.Message, len(messages))
	copy(sorted, messages)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority > sorted[j].Priority // higher priority first; ties keep arrival order
	})
	return sorted
}

// formatInjectOutput formats messages as a <system-reminder> block for
// injection into an agent's prompt via a UserPromptSubmit hook. It priority-
// sorts before the display clamp so both inject render paths
// (renderMailCheckFromAPI and doMailCheckTargetWithFormat) surface higher-
// priority unread first.
func formatInjectOutput(messages []mail.Message) string {
	messages = sortMailByPriority(messages)
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	fmt.Fprintf(&sb, "You have %d unread message(s).\n\n", len(messages))
	limit := len(messages)
	if limit > mailInjectMaxMessages {
		limit = mailInjectMaxMessages
		fmt.Fprintf(&sb, "Showing the first %d message(s) here; run 'gc mail inbox' for the full list.\n\n", limit)
	}
	for _, m := range messages[:limit] {
		// Sanitize attacker-controllable fields (sender identity, subject,
		// body) before interpolating into the <system-reminder> block.
		// Without this, a sender can inject </system-reminder> sequences
		// and break out of the reminder. See gastownhall/gascity#2195.
		from := extmsg.SanitizeForSystemReminder(m.From)
		// verifiedMarker is server-computed (beadmail re-derives Verified from
		// the message's own content against the operator trust key on every
		// read — see beadmail.verifySignedMetadata), never attacker text, so
		// it needs no sanitization. An ordinary "from human"/"from controller"
		// claim — including every message sent before this mechanism existed,
		// and any --from human/--from controller sent without --sign — never
		// carries this marker and must be read exactly as skeptically as
		// before: a caller-supplied claim, not verified evidence.
		verifiedMarker := ""
		if m.Verified && m.VerifiedIdentity != "" {
			verifiedMarker = fmt.Sprintf(" [cryptographically verified %s sender]", m.VerifiedIdentity)
		}
		rawSubject, subjectTruncated := mailInjectSubjectPreview(m.Subject)
		subject := extmsg.SanitizeForSystemReminder(rawSubject)
		rawBody, bodyTruncated := mailInjectBodyPreview(m.Body)
		body := extmsg.SanitizeForSystemReminder(rawBody)
		// A message with no body is not a message whose content went missing:
		// the subject IS the content. `gc mail send <to> -s "text"` and
		// POST /v0/mail with the optional body omitted both produce this shape.
		// Without the substitution it renders as "[subject]: " — a subject in
		// brackets and nothing behind the colon, which reads as lost content
		// and is what made ga-6eukj0 look like a storage bug. Substituting here
		// covers every ingress, since all of them converge on this read path.
		if body == "" {
			body, bodyTruncated = subject, subjectTruncated
		}
		if subject != "" && subject != body {
			fmt.Fprintf(&sb, "- %s from %s%s [%s", m.ID, from, verifiedMarker, subject)
			if subjectTruncated {
				sb.WriteString(" ... [subject truncated]")
			}
			fmt.Fprintf(&sb, "]: %s", body)
		} else {
			fmt.Fprintf(&sb, "- %s from %s%s: %s", m.ID, from, verifiedMarker, body)
		}
		if bodyTruncated {
			sb.WriteString(" ... [preview truncated]")
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("\nRun 'gc mail read <id>' for full details, or 'gc mail inbox' to see all.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func mailInjectSubjectPreview(subject string) (string, bool) {
	return mailInjectTextPreview(subject, mailInjectBodyPreviewSize)
}

func mailInjectBodyPreview(body string) (string, bool) {
	return mailInjectTextPreview(body, mailInjectBodyPreviewSize)
}

func mailInjectTextPreview(text string, limit int) (string, bool) {
	if limit <= 0 {
		return "", strings.TrimSpace(text) != ""
	}

	var sb strings.Builder
	scanned := 0
	pendingSpace := false
	for len(text) > 0 {
		if scanned >= mailInjectPreviewScanSize {
			return sb.String(), true
		}

		r, size := utf8.DecodeRuneInString(text)
		if scanned+size > mailInjectPreviewScanSize {
			return sb.String(), true
		}
		text = text[size:]
		scanned += size

		if unicode.IsSpace(r) || unicode.IsControl(r) {
			if sb.Len() > 0 {
				pendingSpace = true
			}
			continue
		}

		encodedLen := utf8.RuneLen(r)
		if encodedLen < 0 {
			encodedLen = len(string(r))
		}
		needed := encodedLen
		if pendingSpace && sb.Len() > 0 {
			needed++
		}
		if sb.Len()+needed > limit {
			return sb.String(), true
		}
		if pendingSpace && sb.Len() > 0 {
			sb.WriteByte(' ')
			pendingSpace = false
		}
		sb.WriteRune(r)
	}
	return sb.String(), false
}

func defaultMailIdentity() string {
	return defaultMailIdentityCandidates()[0]
}

const controllerMailIdentity = "controller"

func reservedMailSenderIdentity(identifier string) (string, bool) {
	switch normalizeNamedSessionTarget(identifier) {
	case "", "human":
		return "human", true
	case controllerMailIdentity:
		return controllerMailIdentity, true
	default:
		return "", false
	}
}

// defaultMailIdentityCandidates returns ordered non-empty identity candidates
// (GC_SESSION_ID, GC_ALIAS, GC_AGENT), falling back to ["human"] when all are
// unset. Multiple candidates preserve compatibility for sessions whose concrete
// ID is unavailable while still preferring the concrete mailbox when it exists.
func defaultMailIdentityCandidates() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(os.Getenv("GC_SESSION_ID"))
	add(os.Getenv("GC_ALIAS"))
	add(os.Getenv("GC_AGENT"))
	if len(out) == 0 {
		out = append(out, "human")
	}
	return out
}

// managedSessionEnvKeys lists the environment variables the orchestrator
// stamps into a session's process when it spawns it — never set for a human's
// own interactive shell. See internal/session/lifecycle.go and
// cmd/gc/build_desired_state.go (Env["GC_AGENT"] = identity, etc.) and
// bdTelemetryAgentID's use of the same pair for the BEADS_ACTOR-carrying
// counterpart. Their presence, independent of whether the value they carry
// resolves to a live session bead, is what distinguishes "this process is a
// managed session" from "this process is an unmanaged human terminal."
var managedSessionEnvKeys = []string{"GC_SESSION_ID", "GC_ALIAS", "GC_AGENT"}

// ambientManagedSession reports whether any managed-session identity env var
// is set to a non-empty value, regardless of what it names.
func ambientManagedSession() bool {
	for _, key := range managedSessionEnvKeys {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// reservedIdentityEnvClaim reports whether a managed-session identity env var
// is set to a reserved sender name ("human" or "controller") rather than a
// real identity, and returns the offending key. The orchestrator only ever
// assigns these vars a session's own real identity, so a reserved name there
// can only be a deliberate or accidental override — never a legitimate
// fallback — and must not be trusted to authenticate the reserved identity it
// names.
func reservedIdentityEnvClaim() (key string, claimed bool) {
	for _, k := range managedSessionEnvKeys {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			continue
		}
		if _, ok := reservedMailSenderIdentity(v); ok {
			return k, true
		}
	}
	return "", false
}

// isReservedMailIdentity reports whether sender is one of the reserved names
// ("human" or "controller") this gate and [humantrust]-based signing apply
// to. Callers must check this against the FINAL resolved sender — never the
// raw --from string — since resolveMailIdentityWithConfigCached is what
// actually decides a claim resolves to a reserved identity in the first
// place.
func isReservedMailIdentity(sender string) bool {
	return sender == mail.ReservedIdentityHuman || sender == controllerMailIdentity
}

// refuseUnauthenticatedHumanSender decides whether a mail sender that
// resolved to a reserved identity ("human" or "controller") must be refused
// as an unauthenticated impersonation attempt, closing the gap behind the ga
// fabricated-authority incident: a session used "gc mail send --from human
// ..." (and, more subtly, a session whose GC_AGENT/GC_ALIAS was set to the
// literal string "human") to make its own outgoing mail read as an
// authenticated escalation from the real human operator, with nothing in the
// message distinguishing it from the genuine article afterward.
//
// GENERALIZATION NOTE: earlier revisions of this function, and every call
// site, checked literally `sender == "human"` and never considered
// "controller" — meaning a managed session (with its OWN real
// GC_SESSION_ID, no env-stripping required at all) could run
// `gc mail send --from controller ...` and sail through completely
// unrefused, a strictly worse and simpler bypass than the documented
// env-strip limitation below. sender is now the actual resolved identity,
// and every call site gates on [isReservedMailIdentity](sender) instead of a
// literal "human" comparison, so this function is reached — and applies the
// same reasoning — for both reserved names.
//
// Gas City has no channel today that lets a real human operator authenticate a
// "from human" claim — mail is documented untrusted data
// (docs/reference/trust-boundaries.md) and no bridge/extmsg inbound path
// stamps a verified-human marker on mail (extmsg delivers to sessions
// directly; it never calls into the mail package). The only signal available
// to tell a genuine unmanaged human terminal apart from a managed agent
// session is whether the orchestrator stamped this process with a session
// identity at all, so:
//
//   - explicitFrom (an explicit "--from human" on the CLI): refused whenever
//     ANY managed-session env var is set, to ANY value. A real human never
//     needs to pass --from at all ("human" is already the default with none of
//     them set), so there is no legitimate reason for a managed session to
//     name it explicitly, and doing so is precisely the spoof this closes.
//   - the implicit default-sender fallback (no --from; GC_SESSION_ID/GC_ALIAS/
//     GC_AGENT resolution landed on "human"): refused only when one of those
//     vars is itself set to the reserved name "human" or "controller" — a
//     value the orchestrator never assigns to a real identity — since that can
//     only be a deliberate override, not a genuine absence of session context.
//
// LIMITATION (deliberately not oversold as a hardened fix): every signal this
// function checks is an ordinary environment variable the calling process
// fully controls. A session that runs, e.g.,
//
//	env -u GC_SESSION_ID -u GC_ALIAS -u GC_AGENT gc mail send --from human ...
//
// before invoking gc removes every signal this function can see and becomes,
// from gc's point of view, indistinguishable from a genuinely unmanaged human
// shell — the implicit fallback then legitimately defaults to "human" and
// this function does not (and, absent a new signal, cannot) refuse it. This
// was confirmed empirically (a throwaway test proved the bypass, then was
// deleted) rather than assumed.
//
// Closing that would require a signal the session's own process cannot erase
// — e.g. an orchestrator-owned registry of managed-session identities keyed
// by PID/PPID ancestry or a kernel-verified channel (a Unix-socket peer
// credential, a namespace/cgroup boundary) — none of which exists in gascity
// today. The closest analogues (tmux's per-pane `set-environment` table,
// beadmail's instance-token fencing in cmd_hook.go's classifyHookClaimSession)
// all still gate on env-supplied identifiers being present in the first
// place, so they offer no independent proof once GC_SESSION_ID/GC_ALIAS/
// GC_AGENT are stripped, and a PID/PPID walk is itself defeatable by
// setsid/re-parenting (see internal/workspacesvc/orphan_reap.go, which treats
// that re-parenting as a normal, expected event, not an anomaly). Building a
// forge-proof check is real infrastructure work beyond this fix's scope.
//
// This function is therefore defense-in-depth against careless or naive
// misuse — a session that types --from human (or --from controller) without
// thinking, or whose GC_AGENT happens to be set to one of those literal
// strings — not a boundary that withstands a determined adversary who knows
// to strip their own environment, and NOT a substitute for
// gc mail send --sign (see internal/humantrust), which is the actual
// forge-proof mechanism: this function has no way to distinguish a genuinely
// unmanaged human shell from a session that stripped its own env, but
// --sign's private key is never in that session's reach regardless of what
// its environment claims. Treat [CreatedByMetadataKey] the same way: it is a
// same-trust-domain breadcrumb, not independent verification. See
// TestCmdMailSendKnownLimitationEnvStripBypassesHumanGate for the pinned,
// intentionally-unclosed regression case.
func refuseUnauthenticatedHumanSender(sender string, explicitFrom bool) (refuse bool, reason string) {
	if key, claimed := reservedIdentityEnvClaim(); claimed {
		return true, fmt.Sprintf("refusing to send as %q: %s is set to a reserved sender identity, which a managed session may never claim", sender, key)
	}
	if explicitFrom && ambientManagedSession() {
		return true, fmt.Sprintf("refusing --from %s: this process has a managed session identity (GC_SESSION_ID, GC_ALIAS, or GC_AGENT is set) and may not send mail claiming to be the %s operator/orchestrator", sender, sender)
	}
	return false, ""
}

// ambientMailActor returns the identity the runtime environment actually
// assigned to this process, independent of any --from/sender claim:
// BEADS_ACTOR (what a bd-backed store attributes writes to; see
// internal/session/lifecycle.go), then the same GC_SESSION_ID/GC_ALIAS/
// GC_AGENT chain used for default sender resolution. It is recorded as
// [mail.CreatedByMetadataKey] on every message the CLI sends or replies to
// (see mailSendWithProvenance/mailReplyWithProvenance), independent of what
// the message's From claims — including "human" mail, which previously
// recorded no provenance at all. Empty means nothing in the environment
// identifies this process as a managed session.
func ambientMailActor() string {
	for _, key := range append([]string{"BEADS_ACTOR"}, managedSessionEnvKeys...) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// mailSendWithProvenance sends through mp, recording ambientMailActor() as the
// message's createdBy via [mail.SendWithProvenance]. Providers that don't
// implement [mail.ProvenanceRecorder] (exec:, fake, test doubles) fall back to
// plain Send.
func mailSendWithProvenance(mp mail.Provider, from, to, subject, body string) (mail.Message, error) {
	return mail.SendWithProvenance(mp, from, to, subject, body, ambientMailActor())
}

// mailReplyWithProvenance is [mailSendWithProvenance] for replies.
func mailReplyWithProvenance(mp mail.Provider, id, from, subject, body string) (mail.Message, error) {
	return mail.ReplyWithProvenance(mp, id, from, subject, body, ambientMailActor())
}

// isStorelessMailProvider reports whether the configured mail provider
// bypasses the city bead store (exec scripts and test doubles).
func isStorelessMailProvider() bool {
	v := mailProviderName()
	return strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail"
}

// sessionMailboxAddresses delegates to the session-class front-door codec
// (internal/session) so the session-bead metadata vocabulary (alias /
// alias_history / session_name) lives in one place. Its sole remaining caller
// holds a single bead already fetched by id; the list-scan sites now read
// session.Info directly via session.MailboxAddress*FromInfo.
func sessionMailboxAddresses(b beads.Bead) []string {
	return session.MailboxAddresses(b)
}

// The mail identity/target resolver family below reads only session-class beads:
// session-ID resolution, the gc:session enumeration behind named-target matching,
// and mailbox-identity metadata. Its store parameter is therefore named sessStore
// and every caller must hand it a session-class store (cliSessionStore at a CLI
// root, cr.sessionsBeadStore().Store in the controller) — the resolvers do no
// routing of their own, so a [beads.classes.sessions] relocation reaches mail
// identity resolution exactly once, at the root that opened the store. Mail
// *messages* are a different class and travel through mail.Provider, not here.
func resolveMailIdentityCached(sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (string, error) {
	if sender, ok := reservedMailSenderIdentity(identifier); ok {
		return sender, nil
	}
	sessionID, err := resolveSessionID(sessStore, identifier)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			if target, matched, targetErr := resolveLiveConfiguredNamedMailTargetCached(sessStore, identifier, cache); targetErr != nil {
				return "", targetErr
			} else if matched {
				return target.display, nil
			}
			if address, ok := configuredMailboxAddress(identifier); ok {
				return address, nil
			}
		}
		return "", err
	}
	address, err := session.NewStore(beads.SessionStore{Store: sessStore}).MailboxAddress(sessionID)
	if err != nil {
		return "", err
	}
	if address == "" {
		return "", fmt.Errorf("session %q has no mailbox identity", identifier)
	}
	return address, nil
}

func resolveMailIdentityWithConfig(cityPath string, cfg *config.City, sessStore beads.Store, identifier string) (string, error) {
	return resolveMailIdentityWithConfigCached(cityPath, cfg, sessStore, identifier, nil)
}

func resolveMailIdentityWithConfigCached(cityPath string, cfg *config.City, sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (string, error) {
	if sender, ok := reservedMailSenderIdentity(identifier); ok {
		return sender, nil
	}
	if sessStore != nil && cfg != nil {
		sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, identifier)
		if err == nil {
			address, err := session.NewStore(beads.SessionStore{Store: sessStore}).MailboxAddress(sessionID)
			if err != nil {
				return "", err
			}
			if address == "" {
				return "", fmt.Errorf("session %q has no mailbox identity", identifier)
			}
			return address, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if target, matched, targetErr := resolveLiveConfiguredNamedMailTargetCached(sessStore, identifier, cache); targetErr != nil {
		return "", targetErr
	} else if matched {
		return target.display, nil
	}
	if address, ok := configuredMailboxAddressWithConfig(cityPath, cfg, identifier); ok {
		return address, nil
	}
	return resolveMailIdentityCached(sessStore, identifier, cache)
}

func resolveMailRecipientIdentity(cityPath string, cfg *config.City, sessStore beads.Store, identifier string) (string, error) {
	return resolveMailRecipientIdentityCached(cityPath, cfg, sessStore, identifier, nil)
}

func resolveMailRecipientIdentityCached(cityPath string, cfg *config.City, sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (string, error) {
	if normalized := normalizeNamedSessionTarget(identifier); normalized == "" || normalized == "human" {
		return "human", nil
	}
	if sessStore != nil {
		sessionID, err := session.ResolveSessionIDByExactID(sessStore, identifier)
		if err == nil {
			return sessionID, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if target, matched, targetErr := resolveLiveConfiguredNamedMailTargetCached(sessStore, identifier, cache); targetErr != nil {
		return "", targetErr
	} else if matched {
		return target.display, nil
	}
	if normalizeNamedSessionTarget(identifier) == controllerMailIdentity {
		return "", session.ErrSessionNotFound
	}
	return resolveMailIdentityWithConfigCached(cityPath, cfg, sessStore, identifier, cache)
}

func configuredMailboxAddress(identifier string) (string, bool) {
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" || identifier == "human" {
		return "", false
	}
	cityPath, err := resolveCity()
	if err != nil {
		return "", false
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return "", false
	}
	return configuredMailboxAddressWithConfig(cityPath, cfg, identifier)
}

func configuredMailboxAddressWithConfig(cityPath string, cfg *config.City, identifier string) (string, bool) {
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" || identifier == "human" || cfg == nil {
		return "", false
	}
	cityName := loadedCityName(cfg, cityPath)
	spec, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier)
	if err != nil || !ok {
		return "", false
	}
	return spec.Identity, true
}

func listLiveSessionMailboxesCached(sessStore beads.Store, cache *mailIdentitySessionCache) (map[string]bool, error) {
	recipients := map[string]bool{"human": true}
	if sessStore == nil {
		return recipients, nil
	}
	all, err := listMailIdentitySessions(sessStore, cache)
	if err != nil {
		return nil, err
	}
	for _, info := range all {
		// ListAll already filters via IsSessionBeadOrRepairable.
		if info.Closed {
			continue
		}
		if address := session.MailboxAddressFromInfo(info); address != "" {
			recipients[address] = true
		}
	}
	return recipients, nil
}

type resolvedMailTarget struct {
	display    string
	recipients []string
}

// mailIdentitySessionCache memoizes a single gc:session enumeration so that
// repeated identity-resolution attempts (multi-candidate retry, sender +
// recipient resolution in the same command, etc.) share the same broad scan.
// A nil cache disables memoization; the zero value memoizes on first use.
type mailIdentitySessionCache struct {
	mu      sync.Mutex
	list    []session.Info
	fetched bool
}

func ambientMailTargetConfig() (string, *config.City) {
	cityPath, err := resolveCity()
	if err != nil {
		return "", nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return cityPath, nil
	}
	return cityPath, cfg
}

// listMailIdentitySessions memoizes the open session Infos for identity
// resolution. It preserves the pre-typed cache semantics exactly: the default
// direct union with IncludeClosed implicit-false (loadOpenSessionInfos), with the
// per-loop closed filter kept in the callers.
func listMailIdentitySessions(sessStore beads.Store, cache *mailIdentitySessionCache) ([]session.Info, error) {
	if cache == nil {
		return loadOpenSessionInfos(sessStore)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.fetched {
		return cache.list, nil
	}
	list, err := loadOpenSessionInfos(sessStore)
	if err != nil {
		return nil, err
	}
	cache.list = list
	cache.fetched = true
	return list, nil
}

func resolveLiveConfiguredNamedMailTargetCached(sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (resolvedMailTarget, bool, error) {
	identifier = normalizeNamedSessionTarget(identifier)
	if sessStore == nil || identifier == "" || identifier == "human" || strings.Contains(identifier, "/") {
		return resolvedMailTarget{}, false, nil
	}
	all, err := listMailIdentitySessions(sessStore, cache)
	if err != nil {
		return resolvedMailTarget{}, false, err
	}

	matches := make(map[string]resolvedMailTarget)
	order := make([]string, 0, 2)
	for _, info := range all {
		// ListAll already filters via IsSessionBeadOrRepairable.
		if info.Closed {
			continue
		}
		identity := namedSessionIdentityInfo(info)
		if identity == "" || targetBasename(identity) != identifier {
			continue
		}
		addresses := session.MailboxAddressesFromInfo(info)
		if len(addresses) == 0 {
			continue
		}
		display := session.MailboxAddressFromInfo(info)
		if display == "" {
			display = addresses[0]
		}
		if _, ok := matches[display]; ok {
			continue
		}
		matches[display] = resolvedMailTarget{
			display:    display,
			recipients: addresses,
		}
		order = append(order, display)
	}

	switch len(order) {
	case 0:
		return resolvedMailTarget{}, false, nil
	case 1:
		return matches[order[0]], true, nil
	default:
		return resolvedMailTarget{}, true, fmt.Errorf("%w: %q matches %d live configured named sessions: %s",
			session.ErrAmbiguous, identifier, len(order), strings.Join(order, ", "))
	}
}

func resolveMailTargets(sessStore beads.Store, identifier string) (resolvedMailTarget, error) {
	return resolveMailTargetsCached(sessStore, identifier, nil)
}

func resolveMailTargetsWithConfig(cityPath string, cfg *config.City, sessStore beads.Store, identifier string) (resolvedMailTarget, error) {
	return resolveMailTargetsWithConfigCached(cityPath, cfg, sessStore, identifier, nil)
}

func resolveMailTargetsWithConfigCached(cityPath string, cfg *config.City, sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (resolvedMailTarget, error) {
	if normalized := normalizeNamedSessionTarget(identifier); normalized == "" || normalized == "human" {
		return resolvedMailTarget{display: "human", recipients: []string{"human"}}, nil
	}
	if sessStore != nil && cfg != nil {
		sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, identifier)
		if err == nil {
			b, err := sessStore.Get(sessionID)
			if err != nil {
				return resolvedMailTarget{}, err
			}
			addresses := sessionMailboxAddresses(b)
			if len(addresses) == 0 {
				return resolvedMailTarget{}, fmt.Errorf("session %q has no mailbox identity", identifier)
			}
			return resolvedMailTarget{
				display:    addresses[0],
				recipients: addresses,
			}, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return resolvedMailTarget{}, err
		}
	}
	return resolveMailTargetsCached(sessStore, identifier, cache)
}

func resolveMailTargetsCached(sessStore beads.Store, identifier string, cache *mailIdentitySessionCache) (resolvedMailTarget, error) {
	if normalized := normalizeNamedSessionTarget(identifier); normalized == "" || normalized == "human" {
		return resolvedMailTarget{display: "human", recipients: []string{"human"}}, nil
	}
	sessionID, err := resolveSessionID(sessStore, identifier)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			if target, matched, targetErr := resolveLiveConfiguredNamedMailTargetCached(sessStore, identifier, cache); targetErr != nil {
				return resolvedMailTarget{}, targetErr
			} else if matched {
				return target, nil
			}
			if address, ok := configuredMailboxAddress(identifier); ok {
				return resolvedMailTarget{display: address, recipients: []string{address}}, nil
			}
		}
		return resolvedMailTarget{}, err
	}
	addresses, err := session.NewStore(beads.SessionStore{Store: sessStore}).MailboxAddresses(sessionID)
	if err != nil {
		return resolvedMailTarget{}, err
	}
	if len(addresses) == 0 {
		return resolvedMailTarget{}, fmt.Errorf("session %q has no mailbox identity", identifier)
	}
	return resolvedMailTarget{
		display:    addresses[0],
		recipients: addresses,
	}, nil
}

func resolveMailTargetsForCommand(identifier string, stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	if normalized := normalizeNamedSessionTarget(identifier); normalized == "" || normalized == "human" {
		return resolvedMailTarget{display: "human", recipients: []string{"human"}}, true
	}
	if isStorelessMailProvider() {
		return resolveRawMailTargetForStorelessProvider(identifier, stderr, cmdName)
	}
	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		_ = code
		return resolvedMailTarget{}, false
	}
	cityPath, cfg := ambientMailTargetConfig()
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, cliSessionStore(store, cfg, cityPath), identifier)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return resolvedMailTarget{}, false
	}
	return target, true
}

// resolveDefaultMailTargetsForCommand tries each default identity candidate
// against the city's bead store and returns the first that resolves. A
// stale GC_ALIAS on a pool worker would otherwise block inbox access when
// GC_SESSION_ID still matches the bead via session_name.
func resolveDefaultMailTargetsForCommand(stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	candidates := defaultMailIdentityCandidates()
	if len(candidates) == 1 || isStorelessMailProvider() {
		return resolveMailTargetsForCommand(candidates[0], stderr, cmdName)
	}
	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		_ = code
		return resolvedMailTarget{}, false
	}
	// Memoize the gc:session enumeration so multi-candidate retry shares one
	// broad scan instead of issuing one per candidate (ga-q6ct Layer 2).
	cityPath, cfg := ambientMailTargetConfig()
	sessStore := cliSessionStore(store, cfg, cityPath)
	cache := &mailIdentitySessionCache{}
	for _, c := range candidates {
		target, err := resolveMailTargetsWithConfigCached(cityPath, cfg, sessStore, c, cache)
		if err == nil {
			return target, true
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
			return resolvedMailTarget{}, false
		}
	}
	fmt.Fprintf(stderr, "%s: no mail identity resolved (tried %v)\n", cmdName, candidates) //nolint:errcheck // best-effort stderr
	return resolvedMailTarget{}, false
}

// resolveDefaultMailSenderForCommand is the single shared resolver for the
// implicit ($GC_SESSION_ID/$GC_ALIAS/$GC_AGENT-or-"human") default sender used
// by every CLI command that does not take its own explicit --from: gc mail
// reply and gc handoff --target both call this directly, and gc mail send
// calls it whenever --from was not given. The unauthenticated-human refusal
// is gated HERE rather than at each call site so a future caller that resolves
// a default sender through this function is covered automatically instead of
// depending on remembering to add the check again (gc handoff's SendHandoff
// path originally shipped without it — see refuseUnauthenticatedHumanSender's
// doc comment for what this can and cannot catch).
func resolveDefaultMailSenderForCommand(cityPath string, cfg *config.City, sessStore beads.Store, stderr io.Writer, cmdName string) (string, bool) {
	return resolveDefaultMailSenderForCommandCached(cityPath, cfg, sessStore, stderr, cmdName, nil)
}

func resolveDefaultMailSenderForCommandCached(cityPath string, cfg *config.City, sessStore beads.Store, stderr io.Writer, cmdName string, cache *mailIdentitySessionCache) (string, bool) {
	candidates := defaultMailIdentityCandidates()
	for _, c := range candidates {
		sender, err := resolveMailIdentityWithConfigCached(cityPath, cfg, sessStore, c, cache)
		if err == nil {
			// This resolver only ever runs the implicit (no --from) path, so
			// explicitFrom is always false here; an explicit "--from human" (or
			// "--from controller") on gc mail send is refused separately, at
			// that call site, because this function is never reached for it.
			if isReservedMailIdentity(sender) {
				if refuse, reason := refuseUnauthenticatedHumanSender(sender, false); refuse {
					fmt.Fprintf(stderr, "%s: %s\n", cmdName, reason) //nolint:errcheck // best-effort stderr
					return "", false
				}
			}
			return sender, true
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			fmt.Fprintf(stderr, "%s: invalid sender %q: %v\n", cmdName, c, err) //nolint:errcheck // best-effort stderr
			return "", false
		}
	}
	fmt.Fprintf(stderr, "%s: no sender identity resolved (tried %v)\n", cmdName, candidates) //nolint:errcheck // best-effort stderr
	return "", false
}

func resolveMailTargetFromArgs(args []string, stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	if len(args) > 0 {
		return resolveMailTargetsForCommand(args[0], stderr, cmdName)
	}
	return resolveDefaultMailTargetsForCommand(stderr, cmdName)
}

func resolveRawMailTargetForStorelessProvider(identifier string, stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	if !isStorelessMailProvider() {
		return resolvedMailTarget{}, false
	}
	store, err := openMailTargetStore()
	if err != nil {
		if isNoCityStoreError(err) {
			return resolvedMailTarget{display: identifier, recipients: []string{identifier}}, true
		}
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return resolvedMailTarget{}, false
	}
	if err == nil && store != nil {
		cityPath, cfg := ambientMailTargetConfig()
		target, resolveErr := resolveMailTargets(cliSessionStore(store, cfg, cityPath), identifier)
		if resolveErr == nil {
			return target, true
		}
		if !errors.Is(resolveErr, session.ErrSessionNotFound) {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, resolveErr) //nolint:errcheck // best-effort stderr
			return resolvedMailTarget{}, false
		}
	}
	return resolvedMailTarget{display: identifier, recipients: []string{identifier}}, true
}

func isNoCityStoreError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not in a city directory") || strings.Contains(msg, "not a city directory")
}

var openMailTargetStore = tryOpenCityStore

func tryOpenCityStore() (beads.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	return openStoreAtForCity(cityPath, cityPath)
}

func resolveMailAddressForCommand(identifier string, stderr io.Writer, cmdName string) (string, bool) {
	target, ok := resolveMailTargetsForCommand(identifier, stderr, cmdName)
	if !ok {
		return "", false
	}
	return target.display, true
}

func collectMailMessages(fetch func(string) ([]mail.Message, error), recipients []string) ([]mail.Message, error) {
	seen := map[string]mail.Message{}
	order := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		messages, err := fetch(recipient)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if _, ok := seen[message.ID]; !ok {
				order = append(order, message.ID)
			}
			seen[message.ID] = message
		}
	}
	result := make([]mail.Message, 0, len(order))
	for _, id := range order {
		result = append(result, seen[id])
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func collectMailCounts(count func(string) (int, int, error), recipients []string) (int, int, error) {
	total := 0
	unread := 0
	for _, recipient := range recipients {
		recipientTotal, recipientUnread, err := count(recipient)
		if err != nil {
			return 0, 0, err
		}
		total += recipientTotal
		unread += recipientUnread
	}
	return total, unread, nil
}

type multiRecipientMailCounter interface {
	CountRecipients([]string) (int, int, error)
}

func newMailSendCmd(stdout, stderr io.Writer) *cobra.Command {
	var notify bool
	var all bool
	var from string
	var to string
	var subject string
	var message string
	var jsonOut bool
	var sign bool
	var signedEnvelope string
	cmd := &cobra.Command{
		Use:   "send [<to>] [<body>]",
		Short: "Send a message to a session alias or human",
		Long: `Send a message to a session alias or human.

Creates a message bead addressed to the recipient. The sender defaults
to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human". Use --notify to request
a recipient turn after sending. In a managed city, it can request a wake for
a non-running recipient. Unread mail alone does not request a wake.
Use --from to override the sender identity. A managed session (any of
$GC_SESSION_ID, $GC_ALIAS, $GC_AGENT set) is refused if it tries to send as
"human" — neither via --from human nor by having one of those variables
itself set to "human" — since nothing on this host can independently verify
that claim; only a genuinely unmanaged shell may default to "human". This is
a defense-in-depth guard against careless or naive misuse, not a hardened
authentication boundary: a session that deliberately clears its own
GC_SESSION_ID/GC_ALIAS/GC_AGENT before invoking gc is indistinguishable from
an unmanaged shell and is not caught.
Use --to as an alternative to the positional <to> argument.
Use -s/--subject for the summary line and -m/--message for the body text.
Use --all to broadcast to all live sessions (excluding sender and "human").

--sign requires this process itself to hold the operator's private trust
key (see 'gc mail trust init') — meant only for a machine that does not run
the gascity orchestrator or any spawned session. When the key instead lives
on a separate machine (the normal case for a node that runs spawned
sessions), sign there with 'gc mail trust sign' and relay the resulting
token here with --signed-envelope; this process then never needs the
private key at all.`,
		Example: `  gc mail send mayor "Build is green"
  gc mail send mayor -s "Build is green"
  gc mail send myrig/witness -s "Need investigation" -m "Attach logs from the last failed run"
  gc mail send --to mayor "Build is green"
  gc mail send human "Review needed for PR #42"
  gc mail send polecat "Priority task" --notify
  gc mail send --all "Status update: tests passing"
  gc mail send --signed-envelope "$(gc mail trust sign mayor 'approve the deploy')"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := cmdMailSendJSONWithSign(args, notify, all, from, to, subject, message, jsonOut, sign, signedEnvelope, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&notify, "notify", false, "request a recipient turn (including a managed wake if not running), even with earlier unread mail")
	cmd.Flags().BoolVar(&notify, "nudge", false, "alias for --notify")
	_ = cmd.Flags().MarkHidden("nudge")
	cmd.Flags().BoolVar(&all, "all", false, "broadcast to all live sessions (excludes sender and human)")
	cmd.Flags().StringVar(&from, "from", "", "sender identity (default: $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or \"human\"; a managed session cannot claim \"human\")")
	cmd.Flags().StringVar(&to, "to", "", "recipient address (alternative to positional argument)")
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "message subject line")
	cmd.Flags().StringVarP(&message, "message", "m", "", "message body text")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	cmd.Flags().BoolVar(&sign, "sign", false, "cryptographically sign this reserved-identity (human/controller) send with the operator-only trust key THIS PROCESS HOLDS (see 'gc mail trust init'); refused when this process has a managed-session identity or no trust key is provisioned; for a node that does not hold the key, use --signed-envelope instead")
	cmd.Flags().StringVar(&signedEnvelope, "signed-envelope", "", "relay a pre-signed envelope from 'gc mail trust sign' (run on the key-holding machine); pass \"-\" to read the token from stdin; supplies identity/recipient/subject/body itself, so combine with nothing else")
	cmd.MarkFlagsMutuallyExclusive("to", "all")
	cmd.MarkFlagsMutuallyExclusive("sign", "signed-envelope")
	return cmd
}

func newMailInboxCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "inbox [session]",
		Short: "List unread messages (defaults to your inbox)",
		Long: `List all unread messages for a session alias or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human". Pass a session alias to view another inbox.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailInboxWithJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func newMailReadCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "read <id>",
		Short: "Read a message and mark it as read",
		Long: `Display a message and mark it as read.

Shows the full message details (ID, sender, recipient, subject, date, body).
The message stays in the store — use "gc mail archive" to remove it.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailReadWithJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func newMailPeekCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "peek <id>",
		Short: "Show a message without marking it as read",
		Long: `Display a message without marking it as read.

Same output as "gc mail read" but does not change the message's read status.
The message will continue to appear in inbox results.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailPeekWithJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func newMailReplyCmd(stdout, stderr io.Writer) *cobra.Command {
	var subject string
	var message string
	var notify bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "reply <id> [-s subject] [-m body]",
		Short: "Reply to a message",
		Long: `Reply to a message. The reply is addressed to the original sender.

Inherits the thread ID from the original message for conversation tracking.
Use --notify to request a recipient turn after replying. In a managed city,
it can request a wake for a non-running recipient.
Unread mail alone does not request a wake.
Use -s/--subject for the reply subject and -m/--message for the reply body.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdMailReplyJSON(args, subject, message, notify, true, stdout, stderr)
			} else {
				code = cmdMailReply(args, subject, message, notify, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "reply subject line")
	cmd.Flags().StringVarP(&message, "message", "m", "", "reply body text")
	cmd.Flags().BoolVar(&notify, "notify", false, "request a recipient turn (including a managed wake if not running), even with earlier unread mail")
	cmd.Flags().BoolVar(&notify, "nudge", false, "alias for --notify")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	_ = cmd.Flags().MarkHidden("nudge")
	return cmd
}

func newMailMarkReadCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "mark-read <id>",
		Short: "Mark a message as read",
		Long:  `Mark a message as read without displaying it. The message will no longer appear in inbox results.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdMailMarkReadJSON(args, true, stdout, stderr)
			} else {
				code = cmdMailMarkRead(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func newMailMarkUnreadCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "mark-unread <id>",
		Short: "Mark a message as unread",
		Long:  `Mark a message as unread. The message will appear again in inbox results.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdMailMarkUnreadJSON(args, true, stdout, stderr)
			} else {
				code = cmdMailMarkUnread(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func newMailDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "delete <id>...",
		Short: "Delete one or more messages (closes the beads)",
		Long: `Delete one or more messages by closing the beads. Same effect as archive
but with different user intent. When multiple IDs are passed, they are
deleted in a single batch round-trip.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdMailDeleteJSON(args, true, stdout, stderr)
			} else {
				code = cmdMailDelete(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func newMailThreadCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "thread <id>",
		Short: "List all messages in a thread",
		Long:  `Show all messages sharing a thread ID or message ID, ordered by time.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailThreadWithJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func newMailCountCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "count [session]",
		Short: "Show total/unread message count",
		Long: `Show total and unread message counts for a session alias or human.
The recipient defaults to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailCountWithJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

// cmdMailSend is the CLI entry point for sending mail. It opens the provider,
// resolves session mailbox identities, and delegates to doMailSend.
// The to parameter is the --to flag value (empty if not set).
func cmdMailSend(args []string, notify bool, all bool, from string, to string, subject string, message string, stdout, stderr io.Writer) int {
	return cmdMailSendJSON(args, notify, all, from, to, subject, message, false, stdout, stderr)
}

func cmdMailSendJSON(args []string, notify bool, all bool, from string, to string, subject string, message string, jsonOut bool, stdout, stderr io.Writer) int {
	return cmdMailSendJSONWithSign(args, notify, all, from, to, subject, message, jsonOut, false, "", stdout, stderr)
}

// cmdMailSendJSONWithSign is cmdMailSendJSON plus two mutually exclusive
// verified-send surfaces:
//
//   - sign=true: this process itself loads the operator's private trust key
//     (see prepareSignedSend) and signs on the spot. Only valid when this
//     process genuinely holds that key — i.e. it is not running on a node
//     that spawns gc sessions.
//   - signedEnvelope != "": a signature produced elsewhere (by
//     "gc mail trust sign", run on the key-holding machine) is decoded,
//     locally re-verified against this node's already-provisioned PUBLIC
//     key, and relayed into the store. This process never loads or needs
//     the private key.
//
// Both produce a message cryptographically bound to its sender identity,
// recipient, and body via internal/humantrust. Neither set (the default
// from cmdMailSendJSON/cmdMailSend) is byte-for-byte the pre-existing,
// unsigned behavior.
func cmdMailSendJSONWithSign(args []string, notify bool, all bool, from string, to string, subject string, message string, jsonOut bool, sign bool, signedEnvelope string, stdout, stderr io.Writer) int {
	if sign && all {
		fmt.Fprintln(stderr, "gc mail send: --sign cannot be combined with --all (sign one addressed recipient at a time)") //nolint:errcheck // best-effort stderr
		return 1
	}
	if signedEnvelope != "" {
		if all {
			fmt.Fprintln(stderr, "gc mail send: --signed-envelope cannot be combined with --all (relay one addressed recipient at a time)") //nolint:errcheck // best-effort stderr
			return 1
		}
		if from != "" || to != "" || subject != "" || message != "" || len(args) > 0 {
			fmt.Fprintln(stderr, "gc mail send: --signed-envelope supplies its own sender, recipient, subject, and body; combine it with nothing else (notify/json excepted)") //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	mp, code := openCityMailProvider(stderr, "gc mail send")
	if mp == nil {
		return code
	}

	var (
		store           beads.Store
		validRecipients map[string]bool
		cfg             *config.City
	)
	cityPath, err := resolveCity()
	if err == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
		store, err = openStoreAtForCity(cityPath, cityPath)
	}
	// Narrower than isStorelessMailProvider: exec: providers can legitimately
	// run without a city store, but fake/fail still require one for alias
	// resolution in tests. Do not unify with isStorelessMailProvider.
	if err != nil && !strings.HasPrefix(mailProviderName(), "exec:") {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Every read below is session-class (live-mailbox enumeration, sender and
	// recipient identity), so route once here; the resolvers take the routed
	// store. store itself stays the work store for the nudge path.
	var sessStore beads.Store
	if store != nil {
		sessStore = cliSessionStore(store, cfg, cityPath)
	}
	// Memoize the gc:session enumeration so identity resolution (sender +
	// recipient + listLiveSessionMailboxes) shares one broad scan instead of
	// issuing one per call site (ga-q6ct Layer 3).
	idCache := &mailIdentitySessionCache{}
	if store != nil {
		validRecipients, err = listLiveSessionMailboxesCached(sessStore, idCache)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: listing live sessions: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// --signed-envelope carries its own identity, recipient, subject, and
	// body (validated above to be the ONLY addressing info given), so it
	// branches out here before ordinary sender resolution and
	// refuseUnauthenticatedHumanSender: its authenticity comes from a
	// cryptographic signature this node independently re-verifies against
	// its own provisioned public key (see doMailSendEnvelopeJSON), not from
	// an environment-based claim, so the env-var gate does not apply and
	// this process never needs the private key.
	if signedEnvelope != "" {
		rec := openCityRecorder(stderr)
		return doMailSendEnvelopeJSON(mp, rec, validRecipients, cityPath, cfg, sessStore, idCache, signedEnvelope, notify, jsonOut, stdout, stderr)
	}

	sender := from
	explicitFrom := from != ""
	if sender == "" {
		if store != nil {
			var ok bool
			sender, ok = resolveDefaultMailSenderForCommandCached(cityPath, cfg, sessStore, stderr, "gc mail send", idCache)
			if !ok {
				return 1
			}
		} else {
			sender = defaultMailIdentity()
		}
	} else if sender != "human" && store != nil {
		sender, err = resolveMailIdentityWithConfigCached(cityPath, cfg, sessStore, sender, idCache)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: invalid sender %q: %v\n", sender, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if isReservedMailIdentity(sender) {
		if refuse, reason := refuseUnauthenticatedHumanSender(sender, explicitFrom); refuse {
			fmt.Fprintf(stderr, "gc mail send: %s\n", reason) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	var nf nudgeFunc
	if notify && store != nil {
		nf = newMailNudgeFunc(sender)
	}

	// When --to is set, prepend it to args so doMailSend sees [to, body].
	if to != "" && !all {
		args = append([]string{to}, args...)
	}

	// When -s/-m flags provide subject/body, use them.
	if subject != "" || message != "" {
		if all {
			allBody := message
			if allBody == "" && len(args) > 0 {
				allBody = args[0]
			}
			args = []string{subject, allBody}
		} else {
			if len(args) < 1 {
				fmt.Fprintln(stderr, "gc mail send: missing recipient") //nolint:errcheck // best-effort stderr
				return 1
			}
			body := message
			if body == "" && len(args) > 1 {
				body = strings.Join(args[1:], " ")
			}
			args = []string{args[0], subject, body}
		}
	}
	if !all && len(args) > 0 && store != nil {
		canonicalTo, err := resolveMailRecipientIdentityCached(cityPath, cfg, sessStore, args[0], idCache)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: unknown recipient %q: %v\n", args[0], err) //nolint:errcheck // best-effort stderr
			return 1
		}
		args[0] = canonicalTo
		if validRecipients != nil {
			validRecipients[canonicalTo] = true
		}
	}

	if all {
		rec := openCityRecorder(stderr)
		return doMailSendAllJSON(mp, rec, validRecipients, sender, args, nf, jsonOut, stdout, stderr)
	}

	rec := openCityRecorder(stderr)
	if sign {
		return doMailSendSignedJSON(mp, rec, validRecipients, sender, args, nf, jsonOut, stdout, stderr)
	}
	return doMailSendJSON(mp, rec, validRecipients, sender, args, nf, jsonOut, stdout, stderr)
}

// prepareSignedSend loads everything a genuine --sign invocation needs and
// refuses loudly, with an actionable reason, whenever it cannot: sign is
// requested from a process with a managed-session identity, sender is not a
// reserved identity, or no operator trust key is provisioned on this host.
// It never falls back to an unsigned send — a caller that asked for --sign
// and can't get it must see a failure, not a message that looks ordinary.
func prepareSignedSend(sender string, stderr io.Writer) (signer func(to, body string) (signature []byte, issuedAt time.Time), ok bool) {
	if sender != mail.ReservedIdentityHuman && sender != mail.ReservedIdentityController {
		fmt.Fprintf(stderr, "gc mail send: --sign requires --from %s or --from %s (got %q)\n", mail.ReservedIdentityHuman, mail.ReservedIdentityController, sender) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	// Belt-and-suspenders, matching refuseUnauthenticatedHumanSender's style:
	// a spawned session should never legitimately reach for --sign even if a
	// trust key happened to be readable, because it should never BE the
	// process holding the operator's private key. The real boundary is key
	// possession (checked immediately below); this is defense-in-depth on
	// top of it, not instead of it.
	if ambientManagedSession() {
		fmt.Fprintln(stderr, "gc mail send: refusing --sign: this process has a managed session identity (GC_SESSION_ID, GC_ALIAS, or GC_AGENT is set); only a genuinely unmanaged human terminal or an explicitly provisioned trust relay may sign") //nolint:errcheck // best-effort stderr
		return nil, false
	}
	key, err := humantrust.LoadPrivateKey()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: --sign: %v; run 'gc mail trust init' from a genuine human terminal first\n", err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	return func(to, body string) ([]byte, time.Time) {
		issuedAt := time.Now()
		return humantrust.Sign(key, sender, to, body, issuedAt), issuedAt
	}, true
}

// doMailSend creates a message addressed to a recipient. args is [to, subject, body]
// or [to, body] (subject="" if no -s flag). When nudgeFn is non-nil, the
// recipient is nudged after message creation (skipped for "human").
func doMailSend(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, stdout, stderr io.Writer) int {
	return doMailSendJSON(mp, rec, validRecipients, sender, args, nudgeFn, false, stdout, stderr)
}

func doMailSendJSON(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "gc mail send: usage: gc mail send <to> <body>  OR  gc mail send <to> -s <subject> [-m <body>]") //nolint:errcheck // best-effort stderr
		return 1
	}
	to := args[0]

	var subject, body string
	if len(args) >= 3 {
		// [to, subject, body] — from -s/-m flags.
		subject = args[1]
		body = args[2]
	} else {
		// [to, body] — positional arg, no subject.
		body = strings.Join(args[1:], " ")
	}

	if validRecipients != nil && !validRecipients[to] {
		fmt.Fprintf(stderr, "gc mail send: unknown recipient %q\n", to) //nolint:errcheck // best-effort stderr
		return 1
	}

	m, err := mailSendWithProvenance(mp, sender, to, subject, body)
	telemetry.RecordMailOp(context.Background(), "send", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   m.From,
		Subject: m.ID,
		Message: to,
		Payload: mailEventPayload(&m),
	})
	if !jsonOut {
		fmt.Fprintf(stdout, "Sent message %s to %s\n", m.ID, to) //nolint:errcheck // best-effort stdout
	}

	// Nudge recipient if requested and recipient is not human.
	notified := false
	if nudgeFn != nil && to != "human" {
		if err := nudgeFn(to, m.ID); err != nil {
			fmt.Fprintf(stderr, "gc mail send: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		} else {
			notified = true
		}
	}
	if jsonOut {
		summary := summarizeMailMessage(m)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", ID: m.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1), Notified: notified})
	}
	return 0
}

// doMailSendSignedJSON is doMailSendJSON's --sign counterpart: single
// recipient only (no --all), and the created message carries a signature
// [mail.SendSigned] stores verbatim so later reads can independently
// re-verify it (see beadmail.verifySignedMetadata) rather than trusting this
// call's own success. Refuses outright — never falls back to an unsigned
// send — when prepareSignedSend can't produce a signer (wrong/no reserved
// sender, managed-session identity present, or no trust key provisioned) or
// when the configured mail provider does not implement [mail.SignedSender]
// (only the built-in beadmail provider does today; exec: and the HTTP API
// send path do not — see docs/reference/trust-boundaries.md).
func doMailSendSignedJSON(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "gc mail send: usage: gc mail send <to> <body> --sign  OR  gc mail send <to> -s <subject> [-m <body>] --sign") //nolint:errcheck // best-effort stderr
		return 1
	}
	to := args[0]

	var subject, body string
	if len(args) >= 3 {
		subject = args[1]
		body = args[2]
	} else {
		body = strings.Join(args[1:], " ")
	}

	if validRecipients != nil && !validRecipients[to] {
		fmt.Fprintf(stderr, "gc mail send: unknown recipient %q\n", to) //nolint:errcheck // best-effort stderr
		return 1
	}

	signer, ok := prepareSignedSend(sender, stderr)
	if !ok {
		return 1
	}
	signature, issuedAt := signer(to, body)

	m, supported, err := mail.SendSigned(mp, sender, to, subject, body, signature, issuedAt)
	telemetry.RecordMailOp(context.Background(), "send", err)
	if !supported {
		fmt.Fprintf(stderr, "gc mail send: --sign is not supported by the configured mail provider (%q); only the built-in store-backed provider supports verified sends today\n", mailProviderName()) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !m.Verified {
		// Should be unreachable — SendSigned only returns success alongside
		// content that beadToMessage's own verification confirms — but if a
		// future provider implementation ever manages to return ok=true
		// without a message that verifies, fail loudly rather than claim a
		// signed send succeeded when the read side would disagree.
		fmt.Fprintln(stderr, "gc mail send: --sign: provider reported success but the created message does not verify; refusing to report a signed send") //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   m.From,
		Subject: m.ID,
		Message: to,
		Payload: mailEventPayload(&m),
	})
	if !jsonOut {
		fmt.Fprintf(stdout, "Sent verified message %s to %s (signed as %s)\n", m.ID, to, m.VerifiedIdentity) //nolint:errcheck // best-effort stdout
	}

	notified := false
	if nudgeFn != nil && to != "human" {
		if err := nudgeFn(to, m.ID); err != nil {
			fmt.Fprintf(stderr, "gc mail send: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		} else {
			notified = true
		}
	}
	if jsonOut {
		summary := summarizeMailMessage(m)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", ID: m.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1), Notified: notified})
	}
	return 0
}

// doMailSendEnvelopeJSON is the --signed-envelope counterpart to
// doMailSendSignedJSON: instead of this process loading the private key and
// signing on the spot, it decodes a signature produced elsewhere (by
// "gc mail trust sign", run on the machine that actually holds the key) and
// relays it in. This process never loads or needs the private key — only
// the public key already provisioned here via "gc mail trust import".
//
// The recipient is re-resolved through the same alias-canonicalization
// pipeline an ordinary send uses, then the signature is re-verified against
// the resolved recipient (not the envelope's raw one) before anything is
// written: if the signer's <to> does not resolve identically here, this
// fails closed with a clear mismatch error rather than either silently
// storing an unverifiable message or silently renaming the recipient the
// signer authenticated. A message is only ever written once this process
// has independently confirmed — using no secret, only the local public
// key — that the signature is genuinely valid for the message's exact,
// final content.
func doMailSendEnvelopeJSON(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, cityPath string, cfg *config.City, sessStore beads.Store, idCache *mailIdentitySessionCache, envelopeToken string, notify bool, jsonOut bool, stdout, stderr io.Writer) int {
	if envelopeToken == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: --signed-envelope -: reading stdin: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		envelopeToken = strings.TrimSpace(string(raw))
	}
	env, err := humantrust.DecodeEnvelope(envelopeToken)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !isReservedMailIdentity(env.Identity) {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope: identity %q is not a reserved identity (%s or %s)\n", env.Identity, mail.ReservedIdentityHuman, mail.ReservedIdentityController) //nolint:errcheck // best-effort stderr
		return 1
	}

	to := env.To
	if sessStore != nil {
		canonicalTo, err := resolveMailRecipientIdentityCached(cityPath, cfg, sessStore, env.To, idCache)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: --signed-envelope: unknown recipient %q: %v\n", env.To, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		to = canonicalTo
	}
	if validRecipients != nil && !validRecipients[to] {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope: unknown recipient %q\n", to) //nolint:errcheck // best-effort stderr
		return 1
	}

	pub, err := humantrust.LoadPublicKey()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope: %v; run 'gc mail trust import' on this node first\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !humantrust.Verify(pub, env.Identity, to, env.Body, env.IssuedAt, env.Signature) {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope: signature does not verify for resolved recipient %q (signed for %q); sign using the exact recipient this node resolves to, relay within %s, and confirm this node's trusted public key matches the signer's (see 'gc mail trust show')\n", to, env.To, humantrust.MaxSignatureAge) //nolint:errcheck // best-effort stderr
		return 1
	}

	m, supported, err := mail.SendSigned(mp, env.Identity, to, env.Subject, env.Body, env.Signature, env.IssuedAt)
	telemetry.RecordMailOp(context.Background(), "send", err)
	if !supported {
		fmt.Fprintf(stderr, "gc mail send: --signed-envelope is not supported by the configured mail provider (%q); only the built-in store-backed provider supports verified sends today\n", mailProviderName()) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !m.Verified {
		// Should be unreachable — the pre-check above already confirmed the
		// signature verifies against the exact content being stored — but
		// fail loudly rather than report a signed relay succeeded when the
		// read side would disagree.
		fmt.Fprintln(stderr, "gc mail send: --signed-envelope: provider reported success but the created message does not verify; refusing to report a signed send") //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   m.From,
		Subject: m.ID,
		Message: to,
		Payload: mailEventPayload(&m),
	})
	if !jsonOut {
		fmt.Fprintf(stdout, "Sent verified message %s to %s (signed as %s, relayed)\n", m.ID, to, m.VerifiedIdentity) //nolint:errcheck // best-effort stdout
	}

	notified := false
	if notify && to != "human" {
		if err := newMailNudgeFunc(env.Identity)(to, m.ID); err != nil {
			fmt.Fprintf(stderr, "gc mail send: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		} else {
			notified = true
		}
	}
	if jsonOut {
		summary := summarizeMailMessage(m)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", ID: m.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1), Notified: notified})
	}
	return 0
}

// doMailSendAll broadcasts a message to all live session mailboxes (excluding the
// sender and "human"). With --all, args is [subject, body] or [body].
func doMailSendAll(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, stdout, stderr io.Writer) int {
	return doMailSendAllJSON(mp, rec, validRecipients, sender, args, nil, false, stdout, stderr)
}

func doMailSendAllJSON(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail send --all: usage: gc mail send --all <body>") //nolint:errcheck // best-effort stderr
		return 1
	}

	var subject, body string
	if len(args) >= 2 {
		subject = args[0]
		body = args[1]
	} else {
		body = args[0]
	}

	// Collect recipients in sorted order for deterministic output.
	var recipients []string
	for r := range validRecipients {
		if r == sender || r == "human" {
			continue
		}
		recipients = append(recipients, r)
	}
	sort.Strings(recipients)

	if len(recipients) == 0 {
		fmt.Fprintln(stderr, "gc mail send --all: no recipients (all live sessions excluded)") //nolint:errcheck // best-effort stderr
		return 1
	}

	var sent []mailMessageSummary
	notified := false
	for _, to := range recipients {
		m, err := mailSendWithProvenance(mp, sender, to, subject, body)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send --all: sending to %s: %v\n", to, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		rec.Record(events.Event{
			Type:    events.MailSent,
			Actor:   m.From,
			Subject: m.ID,
			Message: to,
			Payload: mailEventPayload(&m),
		})
		sent = append(sent, summarizeMailMessage(m))
		if !jsonOut {
			fmt.Fprintf(stdout, "Sent message %s to %s\n", m.ID, to) //nolint:errcheck // best-effort stdout
		}

		if nudgeFn != nil {
			if err := nudgeFn(to, m.ID); err != nil {
				fmt.Fprintf(stderr, "gc mail send --all: nudge %s failed: %v\n", to, err) //nolint:errcheck // best-effort stderr
			} else {
				notified = true
			}
		}
	}
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", Messages: sent, Count: intRef(len(sent)), Notified: notified})
	}
	return 0
}

// cmdMailInbox is the CLI entry point for checking the inbox.
func cmdMailInbox(args []string, stdout, stderr io.Writer) int {
	return cmdMailInboxWithJSON(args, false, stdout, stderr)
}

func cmdMailInboxWithJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail inbox")
	if mp == nil {
		return code
	}

	target, ok := resolveMailTargetFromArgs(args, stderr, "gc mail inbox")
	if !ok {
		return 1
	}

	return doMailInboxTargetWithJSON(mp, target, jsonOut, stdout, stderr)
}

type mailInboxReader interface {
	Inbox(recipient string) ([]mail.Message, error)
}

// doMailInbox lists unread messages for a recipient.
func doMailInbox(mp mailInboxReader, recipient string, stdout, stderr io.Writer) int {
	return doMailInboxTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, stdout, stderr)
}

func doMailInboxTarget(mp mailInboxReader, target resolvedMailTarget, stdout, stderr io.Writer) int {
	return doMailInboxTargetWithJSON(mp, target, false, stdout, stderr)
}

func doMailInboxTargetWithJSON(mp mailInboxReader, target resolvedMailTarget, jsonOut bool, stdout, stderr io.Writer) int {
	messages, err := collectMailMessages(mp.Inbox, target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail inbox: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		if err := writeCLIJSONLine(stdout, mailInboxJSONResult{
			SchemaVersion: "1",
			Recipient:     target.display,
			Recipients:    jsonRecipients(target),
			Messages:      messages,
		}); err != nil {
			fmt.Fprintf(stderr, "gc mail inbox: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	if len(messages) == 0 {
		fmt.Fprintf(stdout, "No unread messages for %s\n", target.display) //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFROM\tSUBJECT\tBODY") //nolint:errcheck // best-effort stdout
	for _, m := range messages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.From, m.Subject, truncate(m.Body, 60)) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

func cmdMailReadWithJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail read")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailReadWithJSON(mp, rec, args, jsonOut, stdout, stderr)
}

// doMailRead displays a message and marks it as read. Accepts an injected
// provider and recorder for testability.
func doMailRead(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doMailReadWithJSON(mp, rec, args, false, stdout, stderr)
}

func doMailReadWithJSON(mp mail.Provider, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail read: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	m, err := mp.Read(id)
	telemetry.RecordMailOp(context.Background(), "read", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail read: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec.Record(events.Event{
		Type:    events.MailRead,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})

	if jsonOut {
		if err := writeCLIJSONLine(stdout, mailMessageJSONResult{
			SchemaVersion: "1",
			Message:       m,
		}); err != nil {
			fmt.Fprintf(stderr, "gc mail read: %v\n", err) //nolint:errcheck
			return 1
		}
	} else {
		printMessage(m, stdout)
	}

	return 0
}

func cmdMailPeekWithJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	// The missing-ID guard stays PRE-resolve: in the hand-written form it ran
	// before resolveReadTarget, so on the no-args path resolution/provider side
	// effects never happen. Keeping it here preserves that exactly.
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail peek: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	// onResolveErr preserves mail peek's distinctive behavior: a resolve error
	// (including a remote-client build error) falls back to the local read with
	// no error line and no route= log — exactly the old `if err != nil` branch.
	return routeReadCmdWithHooks("mail peek", stderr, readCmdHooks{
		onResolveErr: func(error) int { return doMailPeekFallback(args, jsonOut, stdout, stderr) },
	}, mailPeekAPIClient, func(cityPath string, c *api.Client, nilReason string) int {
		return routeMailPeek(cityPath, args, c, nilReason, jsonOut, stdout, stderr)
	})
}

// mailPeekAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server.
var mailPeekAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeMailPeek dispatches `mail peek` to the supervisor API when a
// controller is up; otherwise falls back to the local mail-provider path.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeMailPeek(_ string, args []string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	id := args[0]
	var cr api.CachedRead[mail.Message]
	return routeRead(c, "mail peek", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.GetMail(id, "")
			return err
		},
		func() int {
			if jsonOut {
				if err := writeCLIJSONLine(stdout, mailMessageJSONResult{
					SchemaVersion: "1",
					Message:       cr.Body,
				}); err != nil {
					fmt.Fprintf(stderr, "gc mail peek: %v\n", err) //nolint:errcheck
					return 1
				}
				return 0
			}
			printMessage(cr.Body, stdout)
			if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
				fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
			}
			return 0
		},
		func() int { return doMailPeekFallback(args, jsonOut, stdout, stderr) },
	)
}

// doMailPeekFallback is the direct-bd path for `gc mail peek`.
func doMailPeekFallback(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail peek")
	if mp == nil {
		return code
	}
	return doMailPeekWithJSON(mp, args, jsonOut, stdout, stderr)
}

// doMailPeek displays a message without marking it as read.
func doMailPeek(mp mail.Provider, args []string, stdout, stderr io.Writer) int {
	return doMailPeekWithJSON(mp, args, false, stdout, stderr)
}

func doMailPeekWithJSON(mp mail.Provider, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail peek: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	m, err := mp.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		if err := writeCLIJSONLine(stdout, mailMessageJSONResult{
			SchemaVersion: "1",
			Message:       m,
		}); err != nil {
			fmt.Fprintf(stderr, "gc mail peek: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	printMessage(m, stdout)
	return 0
}

// cmdMailReply replies to a message.
func cmdMailReply(args []string, subject, message string, notify bool, stdout, stderr io.Writer) int {
	return cmdMailReplyJSON(args, subject, message, notify, false, stdout, stderr)
}

func cmdMailReplyJSON(args []string, subject, message string, notify bool, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail reply: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}

	mp, code := openCityMailProvider(stderr, "gc mail reply")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)

	sender := defaultMailIdentity()
	providerName := mailProviderName()
	var store beads.Store
	var cityPath string
	var cfg *config.City
	var notifySetupErr error
	if sender != "human" || notify {
		switch {
		case strings.HasPrefix(providerName, "exec:"):
			var err error
			cityPath, err = resolveCity()
			if err == nil {
				cfg, _ = loadCityConfig(cityPath, stderr)
				store, err = openStoreAtForCity(cityPath, cityPath)
			}
			if err != nil {
				notifySetupErr = err
				store = nil
			}
		case !isStorelessMailProvider():
			var storeCode int
			store, storeCode = openCityStore(stderr, "gc mail reply")
			if store == nil {
				return storeCode
			}
			var err error
			cityPath, err = resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc mail reply: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			cfg, _ = loadCityConfig(cityPath, stderr)
		}
		if sender != "human" {
			if store != nil {
				resolved, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr, "gc mail reply")
				if !ok {
					return 1
				}
				sender = resolved
			}
		}
	}
	if isReservedMailIdentity(sender) {
		// gc mail reply has no --from flag, so this can only be the implicit
		// default-sender fallback — never an explicit claim.
		if refuse, reason := refuseUnauthenticatedHumanSender(sender, false); refuse {
			fmt.Fprintf(stderr, "gc mail reply: %s\n", reason) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Determine body from remaining args if -m not set.
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}

	var nf nudgeFunc
	if notify && store != nil {
		nf = newMailNudgeFunc(sender)
	} else if notify && strings.HasPrefix(providerName, "exec:") && notifySetupErr != nil {
		fmt.Fprintf(stderr, "gc mail reply: --notify requested but no city store available; nudge skipped: %v\n", notifySetupErr) //nolint:errcheck // best-effort stderr
	}

	return doMailReplyJSON(mp, rec, args[0], sender, subject, body, nf, jsonOut, stdout, stderr)
}

// doMailReply creates a reply to an existing message.
func doMailReply(mp mail.Provider, rec events.Recorder, id, sender, subject, body string, nudgeFn nudgeFunc, stdout, stderr io.Writer) int {
	return doMailReplyJSON(mp, rec, id, sender, subject, body, nudgeFn, false, stdout, stderr)
}

func doMailReplyJSON(mp mail.Provider, rec events.Recorder, id, sender, subject, body string, nudgeFn nudgeFunc, jsonOut bool, stdout, stderr io.Writer) int {
	reply, err := mailReplyWithProvenance(mp, id, sender, subject, body)
	telemetry.RecordMailOp(context.Background(), "reply", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail reply: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailReplied,
		Actor:   reply.From,
		Subject: reply.ID,
		Message: reply.To,
		Payload: mailEventPayload(&reply),
	})
	if !jsonOut {
		fmt.Fprintf(stdout, "Replied to %s — sent message %s to %s\n", id, reply.ID, reply.To) //nolint:errcheck // best-effort stdout
	}

	notified := false
	if nudgeFn != nil && reply.To != "human" {
		if err := nudgeFn(reply.To, reply.ID); err != nil {
			fmt.Fprintf(stderr, "gc mail reply: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		} else {
			notified = true
		}
	}
	if jsonOut {
		summary := summarizeMailMessage(reply)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail reply", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.reply", Action: "reply", ID: reply.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1), Notified: notified})
	}
	return 0
}

// cmdMailMarkRead marks a message as read.
func cmdMailMarkRead(args []string, stdout, stderr io.Writer) int {
	return cmdMailMarkReadJSON(args, false, stdout, stderr)
}

func cmdMailMarkReadJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail mark-read")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailMarkReadJSON(mp, rec, args, jsonOut, stdout, stderr)
}

// doMailMarkRead marks a message as read.
func doMailMarkRead(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doMailMarkReadJSON(mp, rec, args, false, stdout, stderr)
}

func doMailMarkReadJSON(mp mail.Provider, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail mark-read: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	if err := mp.MarkRead(id); err != nil {
		telemetry.RecordMailOp(context.Background(), "mark_read", err)
		fmt.Fprintf(stderr, "gc mail mark-read: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "mark_read", nil)
	rec.Record(events.Event{
		Type:    events.MailMarkedRead,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail mark-read", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.mark-read", Action: "mark-read", ID: id, IDs: []string{id}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Marked %s as read\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailMarkUnread marks a message as unread.
func cmdMailMarkUnread(args []string, stdout, stderr io.Writer) int {
	return cmdMailMarkUnreadJSON(args, false, stdout, stderr)
}

func cmdMailMarkUnreadJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail mark-unread")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailMarkUnreadJSON(mp, rec, args, jsonOut, stdout, stderr)
}

// doMailMarkUnread marks a message as unread.
func doMailMarkUnread(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doMailMarkUnreadJSON(mp, rec, args, false, stdout, stderr)
}

func doMailMarkUnreadJSON(mp mail.Provider, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail mark-unread: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	if err := mp.MarkUnread(id); err != nil {
		telemetry.RecordMailOp(context.Background(), "mark_unread", err)
		fmt.Fprintf(stderr, "gc mail mark-unread: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "mark_unread", nil)
	rec.Record(events.Event{
		Type:    events.MailMarkedUnread,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail mark-unread", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.mark-unread", Action: "mark-unread", ID: id, IDs: []string{id}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Marked %s as unread\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailDelete deletes a message.
func cmdMailDelete(args []string, stdout, stderr io.Writer) int {
	return cmdMailDeleteJSON(args, false, stdout, stderr)
}

func cmdMailDeleteJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail delete")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailDeleteJSON(mp, rec, args, jsonOut, stdout, stderr)
}

// doMailDelete deletes one or more message beads (same as archive but
// different intent). Single-id behavior matches the pre-batch CLI
// byte-for-byte; multi-id uses mp.DeleteMany to preserve provider delete
// semantics.
func doMailDelete(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doMailDeleteJSON(mp, rec, args, false, stdout, stderr)
}

func doMailDeleteJSON(mp mail.Provider, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	args = splitMessageIDArgs(args)
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail delete: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(args) == 1 {
		if jsonOut {
			return doMailDeleteSingleJSON(mp, rec, args[0], true, stdout, stderr)
		}
		return doMailDeleteSingle(mp, rec, args[0], stdout, stderr)
	}
	if jsonOut {
		return doMailDeleteManyJSON(mp, rec, args, true, stdout, stderr)
	}
	return doMailDeleteMany(mp, rec, args, stdout, stderr)
}

func doMailDeleteSingle(mp mail.Provider, rec events.Recorder, id string, stdout, stderr io.Writer) int {
	return doMailDeleteSingleJSON(mp, rec, id, false, stdout, stderr)
}

func doMailDeleteSingleJSON(mp mail.Provider, rec events.Recorder, id string, jsonOut bool, stdout, stderr io.Writer) int {
	if err := mp.Delete(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			if jsonOut {
				return writeCLIJSONLineOrExit(stdout, stderr, "gc mail delete", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.delete", Action: "delete", ID: id, IDs: []string{id}, Count: intRef(0), AlreadyDone: true})
			}
			fmt.Fprintf(stdout, "Already deleted %s\n", id) //nolint:errcheck // best-effort stdout
			return 0
		}
		telemetry.RecordMailOp(context.Background(), "delete", err)
		fmt.Fprintf(stderr, "gc mail delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "delete", nil)
	rec.Record(events.Event{
		Type:    events.MailDeleted,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail delete", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.delete", Action: "delete", ID: id, IDs: []string{id}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Deleted message %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

func doMailDeleteMany(mp mail.Provider, rec events.Recorder, ids []string, stdout, stderr io.Writer) int {
	return doMailDeleteManyJSON(mp, rec, ids, false, stdout, stderr)
}

func doMailDeleteManyJSON(mp mail.Provider, rec events.Recorder, ids []string, jsonOut bool, stdout, stderr io.Writer) int {
	results, err := mp.DeleteMany(ids)
	if err != nil {
		telemetry.RecordMailOp(context.Background(), "delete", err)
		fmt.Fprintf(stderr, "gc mail delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	exit := 0
	deleted := 0
	already := 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			deleted++
			telemetry.RecordMailOp(context.Background(), "delete", nil)
			rec.Record(events.Event{
				Type:    events.MailDeleted,
				Actor:   eventActor(),
				Subject: r.ID,
				Payload: mailEventPayload(nil),
			})
			if !jsonOut {
				fmt.Fprintf(stdout, "Deleted message %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		case errors.Is(r.Err, mail.ErrAlreadyArchived):
			already++
			if !jsonOut {
				fmt.Fprintf(stdout, "Already deleted %s\n", r.ID) //nolint:errcheck // best-effort stdout
			}
		default:
			telemetry.RecordMailOp(context.Background(), "delete", r.Err)
			fmt.Fprintf(stderr, "gc mail delete %s: %v\n", r.ID, r.Err) //nolint:errcheck // best-effort stderr
			exit = 1
		}
	}
	if jsonOut && exit == 0 {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail delete", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.delete", Action: "delete", IDs: ids, Count: intRef(deleted), AlreadyDone: already == len(ids)})
	}
	return exit
}

func cmdMailThreadWithJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail thread")
	if mp == nil {
		return code
	}
	return doMailThreadWithJSON(mp, args, jsonOut, stdout, stderr)
}

// doMailThread shows all messages in a thread.
func doMailThread(mp mail.Provider, args []string, stdout, stderr io.Writer) int {
	return doMailThreadWithJSON(mp, args, false, stdout, stderr)
}

func doMailThreadWithJSON(mp mail.Provider, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail thread: missing thread or message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := strings.TrimSpace(args[0])
	if id == "" {
		fmt.Fprintln(stderr, "gc mail thread: missing thread or message ID") //nolint:errcheck // best-effort stderr
		return 1
	}

	msgs, err := mp.Thread(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail thread: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if msgs == nil {
		msgs = []mail.Message{}
	}

	if jsonOut {
		if err := writeCLIJSONLine(stdout, mailThreadJSONResult{
			SchemaVersion: "1",
			ThreadID:      canonicalMailThreadID(id, msgs),
			Messages:      msgs,
		}); err != nil {
			fmt.Fprintf(stderr, "gc mail thread: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	if len(msgs) == 0 {
		fmt.Fprintf(stdout, "No messages in thread %s\n", id) //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFROM\tTO\tSUBJECT\tSENT\tREAD") //nolint:errcheck // best-effort stdout
	for _, m := range msgs {
		readStr := " "
		if m.Read {
			readStr = "✓"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.From, m.To, m.Subject, //nolint:errcheck // best-effort stdout
			m.CreatedAt.Format("2006-01-02 15:04"), readStr)
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

func canonicalMailThreadID(fallback string, msgs []mail.Message) string {
	for _, msg := range msgs {
		if strings.TrimSpace(msg.ThreadID) != "" {
			return msg.ThreadID
		}
	}
	return fallback
}

func cmdMailCountWithJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		return doMailCountFallback(args, jsonOut, stdout, stderr)
	}
	c, reason := mailCountAPIClient(cityPath)
	return routeMailCount(cityPath, args, c, reason, jsonOut, stdout, stderr)
}

// mailCountAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server.
var mailCountAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeMailCount dispatches `mail count` to the supervisor API when a
// controller is up; otherwise falls back to the local mail-provider path.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeMailCount(_ string, args []string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	recipient := defaultMailIdentity()
	if len(args) > 0 {
		recipient = strings.TrimSpace(args[0])
	}
	var cr api.CachedRead[api.MailCountView]
	return routeRead(c, "mail count", nilReason, stderr,
		func() error {
			var err error
			if cr, err = c.CountMail(recipient, ""); err != nil {
				return err
			}
			if mailCountHasPartial(cr.Body) {
				return errorAfterFetch{Detail: mailCountPartialErrorDetail(cr.Body)}
			}
			return nil
		},
		func() int {
			if jsonOut {
				if err := writeCLIJSONLine(stdout, mailCountJSONResult{
					SchemaVersion: "1",
					Recipient:     recipient,
					Recipients:    []string{recipient},
					Total:         cr.Body.Total,
					Unread:        cr.Body.Unread,
				}); err != nil {
					fmt.Fprintf(stderr, "gc mail count: %v\n", err) //nolint:errcheck
					return 1
				}
				return 0
			}
			fmt.Fprintf(stdout, "%d total, %d unread for %s\n", cr.Body.Total, cr.Body.Unread, recipient) //nolint:errcheck // best-effort stdout
			if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
				fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
			}
			return 0
		},
		func() int { return doMailCountFallback(args, jsonOut, stdout, stderr) },
	)
}

// doMailCountFallback is the direct-bd path for `gc mail count`.
func doMailCountFallback(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail count")
	if mp == nil {
		return code
	}

	target, ok := resolveMailTargetFromArgs(args, stderr, "gc mail count")
	if !ok {
		return 1
	}

	return doMailCountTargetWithJSON(mp, target, jsonOut, stdout, stderr)
}

// doMailCount displays total/unread message counts.
func doMailCount(mp mail.Provider, recipient string, stdout, stderr io.Writer) int {
	return doMailCountTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, stdout, stderr)
}

func doMailCountTarget(mp mail.Provider, target resolvedMailTarget, stdout, stderr io.Writer) int {
	return doMailCountTargetWithJSON(mp, target, false, stdout, stderr)
}

func doMailCountTargetWithJSON(mp mail.Provider, target resolvedMailTarget, jsonOut bool, stdout, stderr io.Writer) int {
	var total, unread int
	var err error
	if counter, ok := mp.(multiRecipientMailCounter); ok {
		total, unread, err = counter.CountRecipients(target.recipients)
	} else {
		total, unread, err = collectMailCounts(mp.Count, target.recipients)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc mail count: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, mailCountJSONResult{
			SchemaVersion: "1",
			Recipient:     target.display,
			Recipients:    jsonRecipients(target),
			Total:         total,
			Unread:        unread,
		}); err != nil {
			fmt.Fprintf(stderr, "gc mail count: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%d total, %d unread for %s\n", total, unread, target.display) //nolint:errcheck // best-effort stdout
	return 0
}

func jsonRecipients(target resolvedMailTarget) []string {
	if len(target.recipients) == 0 {
		return []string{}
	}
	return append([]string(nil), target.recipients...)
}

// printMessage displays a message's full details.
func printMessage(m mail.Message, stdout io.Writer) {
	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("ID:       %s", m.ID))
	w(fmt.Sprintf("From:     %s", m.From))
	w(fmt.Sprintf("To:       %s", m.To))
	if m.Subject != "" {
		w(fmt.Sprintf("Subject:  %s", m.Subject))
	}
	w(fmt.Sprintf("Sent:     %s", m.CreatedAt.Format("2006-01-02 15:04:05")))
	if m.Body != "" {
		w(fmt.Sprintf("Body:     %s", m.Body))
	}
}

// truncate shortens s to n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// mailEventRig returns the rig name for mail event payloads.
// Reads GC_RIG (set for agents running in rig context).
func mailEventRig() string {
	return os.Getenv("GC_RIG")
}

// mailEventPayload builds a JSON payload for mail events so SSE consumers
// (e.g. dashboard clients) can route updates to the correct rig.
// For sent/replied events, pass the full message; for state changes pass nil.
func mailEventPayload(msg *mail.Message) json.RawMessage {
	m := map[string]any{"rig": mailEventRig()}
	if msg != nil {
		m["message"] = msg
	}
	b, _ := json.Marshal(m)
	return b
}
