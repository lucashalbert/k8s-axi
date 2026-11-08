package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
	"github.com/lucashalbert/axi-sdk-go/redact"
)

const (
	// maxEventScan bounds how many events one command reads. The API server
	// cannot return the newest events first, so the newest are found by
	// reading this many and sorting.
	maxEventScan = 1000
	// messageWidth cuts event messages in the compact view.
	messageWidth = 200
)

// eventQuery selects events: everything in a namespace, or those about one
// object (by UID when known, else by kind and name).
type eventQuery struct {
	Namespace string // empty means all namespaces
	Kind      string
	Name      string
	UID       string
	Type      string        // Normal, Warning or empty for both
	Since     time.Duration // 0 means no age limit
}

// eventResult holds the matching events, newest first. Cut is true when the
// scan limit was hit, so the newest events may be missing.
type eventResult struct {
	Events []corev1.Event
	Cut    bool
}

func (q eventQuery) selector() string {
	var terms []string
	if q.Kind != "" {
		terms = append(terms, "involvedObject.kind="+q.Kind)
	}
	if q.Name != "" {
		terms = append(terms, "involvedObject.name="+q.Name)
	}
	if q.UID != "" {
		terms = append(terms, "involvedObject.uid="+q.UID)
	}
	if q.Type != "" {
		terms = append(terms, "type="+q.Type)
	}
	return strings.Join(terms, ",")
}

func (q eventQuery) matches(e *corev1.Event) bool {
	switch {
	case q.UID != "" && string(e.InvolvedObject.UID) != q.UID:
		return false
	case q.UID == "" && q.Kind != "" && e.InvolvedObject.Kind != q.Kind:
		return false
	case q.UID == "" && q.Name != "" && e.InvolvedObject.Name != q.Name:
		return false
	case q.Type != "" && e.Type != q.Type:
		return false
	case q.Since > 0 && eventTime(e).Before(now().Add(-q.Since)):
		return false
	}
	return true
}

// Events reads the events q selects, newest first. The selector is sent to
// the server and applied again here, so a server that ignores it cannot
// widen the result.
func (c *Clients) Events(ctx context.Context, q eventQuery) (eventResult, error) {
	l, err := c.Typed.CoreV1().Events(q.Namespace).List(ctx, metav1.ListOptions{FieldSelector: q.selector(), Limit: maxEventScan})
	if err != nil {
		return eventResult{}, err
	}
	res := eventResult{Cut: l.Continue != ""}
	for i := range l.Items {
		if q.matches(&l.Items[i]) {
			res.Events = append(res.Events, l.Items[i])
		}
	}
	sort.SliceStable(res.Events, func(i, j int) bool {
		ti, tj := eventTime(&res.Events[i]), eventTime(&res.Events[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return res.Events[i].Namespace+"/"+res.Events[i].Name < res.Events[j].Namespace+"/"+res.Events[j].Name
	})
	return res, nil
}

// eventTime is the latest moment the event was seen, across the old
// (lastTimestamp), new (eventTime, series) and creation timestamps.
func eventTime(e *corev1.Event) time.Time {
	t := e.CreationTimestamp.Time
	for _, c := range []time.Time{e.LastTimestamp.Time, e.EventTime.Time, seriesTime(e)} {
		if c.After(t) {
			t = c
		}
	}
	return t
}

func eventFirstSeen(e *corev1.Event) time.Time {
	switch {
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func seriesTime(e *corev1.Event) time.Time {
	if e.Series == nil {
		return time.Time{}
	}
	return e.Series.LastObservedTime.Time
}

func eventCount(e *corev1.Event) int32 {
	switch {
	case e.Series != nil && e.Series.Count > 0:
		return e.Series.Count
	case e.Count > 0:
		return e.Count
	}
	return 1
}

type eventRow struct {
	Namespace string `json:"namespace,omitempty"`
	Age       string `json:"age"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Object    string `json:"object"`
	Count     int32  `json:"count"`
	Message   string `json:"message"`
}

type eventRowFull struct {
	Namespace string `json:"namespace,omitempty"`
	Age       string `json:"age"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Object    string `json:"object"`
	Count     int32  `json:"count"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Component string `json:"component"`
	UID       string `json:"uid"`
	Message   string `json:"message"`
}

// eventRows renders events as table rows. Compact rows cut the message.
func eventRows(evs []corev1.Event, showNS, full bool) any {
	if full {
		rows := make([]eventRowFull, 0, len(evs))
		for i := range evs {
			e := &evs[i]
			r := eventRowFull{Age: age(eventTime(e)), Type: e.Type, Reason: e.Reason, Object: objectRef(e), Count: eventCount(e),
				FirstSeen: eventFirstSeen(e).UTC().Format(time.RFC3339), LastSeen: eventTime(e).UTC().Format(time.RFC3339),
				Component: component(e), UID: string(e.InvolvedObject.UID), Message: eventMessage(e.Message)}
			if showNS {
				r.Namespace = e.Namespace
			}
			rows = append(rows, r)
		}
		return rows
	}
	rows := make([]eventRow, 0, len(evs))
	for i := range evs {
		e := &evs[i]
		r := eventRow{Age: age(eventTime(e)), Type: e.Type, Reason: e.Reason, Object: objectRef(e), Count: eventCount(e),
			Message: truncate(eventMessage(e.Message), messageWidth)}
		if showNS {
			r.Namespace = e.Namespace
		}
		rows = append(rows, r)
	}
	return rows
}

func eventMessage(s string) string {
	return redact.Mask(strings.Join(strings.Fields(s), " "))
}

func objectRef(e *corev1.Event) string { return e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name }

func component(e *corev1.Event) string {
	if e.Source.Component != "" {
		return e.Source.Component
	}
	return e.ReportingController
}

func warningCount(evs []corev1.Event) int {
	n := 0
	for i := range evs {
		if evs[i].Type == corev1.EventTypeWarning {
			n++
		}
	}
	return n
}

type eventsOut struct {
	Scope     string   `json:"scope"`
	Count     int      `json:"count"`
	Total     int      `json:"total"`
	Warnings  int      `json:"warnings"`
	Truncated bool     `json:"truncated"`
	Note      string   `json:"note,omitempty"`
	Events    any      `json:"events"`
	Next      []string `json:"next,omitempty"`
}

func (g *globals) eventsCmd() *cobra.Command {
	var all bool
	var typ string
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "events [KIND NAME | KIND/NAME]",
		Short: "Show recent events for a namespace or one object, newest first",
		Long: "Show events for the namespace (-A for all namespaces), or only those about one object.\n" +
			"Events are sorted newest first; 'warnings' counts the Warning events among all that match, and --type Warning shows only them.\n" +
			"The API server keeps events for about an hour by default. At most " + fmt.Sprint(maxEventScan) + " matching events are read; the result is cut at --limit rows and says so.",
		Example: "  k8s-axi events -n kube-system --type Warning\n  k8s-axi events pod/web-6d5f8 --since 30m",
		Args:    argRange(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			typ, err = eventType(typ)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("since") && since <= 0 {
				return axierr.Usage("events", "--since must be a positive duration such as 30m or 2h", "pass for example --since 30m")
			}
			var kind, name string
			if len(args) > 0 {
				if kind, name, err = splitTarget(args); err != nil {
					return err
				}
				if name == "" {
					return axierr.Usage("events", "an object needs a name: KIND NAME or KIND/NAME", "write 'k8s-axi events pod web' or 'k8s-axi events pod/web'")
				}
			}
			c, err := g.clients()
			if err != nil {
				return err
			}
			var r Resource
			namespaced := true
			if kind != "" {
				r, err = c.Resolve(kind)
				if err != nil {
					return err
				}
				namespaced = r.Namespaced
			}
			ns, err := scopeNamespace(cmd, c, all, namespaced)
			if err != nil {
				return err
			}
			q := eventQuery{Namespace: ns, Type: typ, Since: since}
			scope := "namespace/" + ns
			if ns == "" {
				scope = "all namespaces"
			}
			if kind != "" {
				q.Kind, q.Name = r.Kind, name
				if !r.Namespaced {
					q.Namespace = ""
				}
				if !r.Namespaced || ns != "" { // the UID tells a recreated object from its predecessor
					if o, err := c.Get(cmd.Context(), r, ns, name); err == nil {
						q.UID = string(o.GetUID())
					}
				}
				scope = strings.ToLower(r.Kind) + "/" + name
				if q.Namespace != "" {
					scope += " in " + q.Namespace
				}
			}
			res, err := c.Events(cmd.Context(), q)
			if err != nil {
				return apiError("events", "context/"+c.Context+" "+scope, err, "list", "events")
			}
			return output.Render(cmd.OutOrStdout(), g.eventsOut(scope, q, res), f)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "events across all namespaces")
	cmd.Flags().StringVar(&typ, "type", "", "only events of this type: Normal or Warning")
	cmd.Flags().DurationVar(&since, "since", 0, "only events seen within this duration, for example 30m")
	return cmd
}

// eventType normalises --type to Normal, Warning or empty.
func eventType(s string) (string, error) {
	switch strings.ToLower(s) {
	case "":
		return "", nil
	case "normal":
		return corev1.EventTypeNormal, nil
	case "warning":
		return corev1.EventTypeWarning, nil
	}
	return "", axierr.Usage("events", fmt.Sprintf("unknown event type %q", s), "use --type Normal or --type Warning")
}

func (g *globals) eventsOut(scope string, q eventQuery, res eventResult) eventsOut {
	evs := res.Events
	out := eventsOut{Scope: scope, Total: len(evs), Warnings: warningCount(evs), Truncated: res.Cut}
	if g.limit > 0 && len(evs) > g.limit {
		evs = evs[:g.limit]
		out.Truncated = true
	}
	out.Count = len(evs)
	out.Events = eventRows(evs, q.Namespace == "", g.full)

	var notes []string
	if out.Count < out.Total {
		notes = append(notes, fmt.Sprintf("showing %d of %d; use --limit 0 to see all, or narrow with --type, --since or an object", out.Count, out.Total))
	}
	if res.Cut {
		notes = append(notes, fmt.Sprintf("more than %d events matched, so the newest events may be missing; narrow with an object, --type or --since", maxEventScan))
	}
	switch {
	case out.Total == 0:
		out.Note = "no events in " + scope + "; the API server keeps events for about an hour"
	case len(notes) > 0:
		out.Note = "truncated: " + strings.Join(notes, "; ")
	}
	for i := range res.Events {
		if e := &res.Events[i]; e.Type == corev1.EventTypeWarning {
			out.Next = []string{g.cmdline("inspect", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name) + nsFlag(e.Namespace)}
			break
		}
	}
	return out
}
