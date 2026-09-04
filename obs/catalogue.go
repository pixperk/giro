package obs

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/pixperk/giro/storage"
)

// A metrics catalogue, and why a ledger needs one more than most systems do.
//
// Cardinality is the failure mode here. The labels a ledger reaches for first
// are account addresses, and addresses are unbounded -- one series per
// customer, discovered in production, usually during the incident the
// dashboard was built for. So the instruments this package creates are
// enumerable, and their cardinality is arithmetic you can do before deploying
// rather than a bill you find out about afterwards.
//
// It also answers the duller question that comes up when someone builds the
// first dashboard: what exists, what is it called, and what may I group by.

// Metric describes one instrument this package emits.
type Metric struct {
	Name   string
	Kind   string // counter | histogram
	Unit   string
	Labels []string
	Help   string
}

// Metrics is every instrument, in the order a dashboard would want them: what
// happened, then what was refused, then how it behaved under load.
//
// Written out rather than derived from the OpenTelemetry SDK, because the
// point is to state the intended shape. A catalogue generated from the code
// agrees with the code by construction and would have caught nothing.
var Metrics = []Metric{
	{
		Name: "giro.transactions", Kind: "counter", Unit: "{transaction}",
		Labels: []string{"giro.ledger", "giro.asset"},
		Help:   "Transactions committed. Counted once per asset, so a conversion increments twice.",
	},
	{
		Name: "giro.refusals", Kind: "counter", Unit: "{transaction}",
		Labels: []string{"giro.ledger", "giro.asset", "giro.reason"},
		Help:   "Transactions declined. Not errors. Group by reason: the causes have different owners.",
	},
	{
		Name: "giro.commit.duration", Kind: "histogram", Unit: "s",
		Labels: []string{"giro.ledger"},
		Help:   "End to end commit latency, including retries and the backoff between them.",
	},
	{
		Name: "giro.lock.wait", Kind: "histogram", Unit: "s",
		Labels: []string{"giro.ledger"},
		Help:   "Time inside the row locking statement. Every deposit locks world, so this is where the hot row appears first.",
	},
	{
		Name: "giro.commit.attempts", Kind: "histogram", Unit: "{attempt}",
		Labels: []string{"giro.ledger"},
		Help:   "Attempts a commit needed. 1 unless it lost a deadlock and started again.",
	},
	{
		Name: "giro.commit.restarts", Kind: "counter", Unit: "{restart}",
		Labels: []string{"giro.ledger"},
		Help:   "Commits restarted after a deadlock. Sorted lock ordering should keep this near zero.",
	},
	{
		Name: "giro.postings", Kind: "histogram", Unit: "{posting}",
		Labels: []string{"giro.ledger"},
		Help:   "Postings per transaction.",
	},
}

// Cardinality estimates how many series these instruments produce.
//
// Both inputs are things an operator knows: how many ledgers are running and
// how many assets are registered across them. Neither grows with traffic, and
// that is the property worth checking -- an estimate that needs to know the
// number of customers is describing a design mistake.
func Cardinality(ledgers, assets int) int {
	total := 0
	for _, m := range Metrics {
		series := 1
		for _, l := range m.Labels {
			switch l {
			case "giro.ledger":
				series *= ledgers
			case "giro.asset":
				series *= assets
			case "giro.reason":
				series *= len(storage.RefusalCauses)
			}
		}
		total += series
	}
	return total
}

// WriteCatalogue prints the catalogue and what it will cost, for
// `giro inspect metrics`.
func WriteCatalogue(w io.Writer, ledgers, assets int) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "METRIC\tKIND\tUNIT\tLABELS"); err != nil {
		return err
	}
	for _, m := range Metrics {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			m.Name, m.Kind, m.Unit, strings.Join(m.Labels, ", ")); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	_, err := fmt.Fprintf(w, `
%d ledgers x %d assets x %d refusal reasons = about %d series.

No metric is labelled by account address. Addresses are unbounded and would
give you one series per customer; they are recorded on spans instead, where
one exists per request rather than per series.
`, ledgers, assets, len(storage.RefusalCauses), Cardinality(ledgers, assets))
	return err
}
