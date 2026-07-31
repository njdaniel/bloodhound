package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/njdaniel/bloodhound/internal/orchestrator"
)

// cost prints what a case spent, per phase and in total. The numbers come from
// summing the case's checkpoints — there is no separate ledger to drift out of
// step with them (spec 002 §4.2).
//
// It runs no phase and writes nothing, so it can only ever exit 0, 2, or the
// I/O-fault half of 1: a bad invocation, an unknown case, and an undecodable
// one are all refusals it inherits from the readers it calls, and the only
// faults left are a case file it may not read and a stdout it cannot write
// (issue #45). It needs no exit-code rule of its own.
func (a *app) cost(args []string) error {
	fs := a.newFlagSet("cost")
	work := fs.String("work", a.envOr("BLOODHOUND_WORK", DefaultWorkRoot), "work root directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if err := checkWorkRoot(*work); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: cost needs exactly one case ID", errUsage)
	}
	caseID := fs.Arg(0)

	c, checkpoints, total, err := orchestrator.Cost(*work, caseID)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "case %s (%s) — phase %s\n", c.ID, orPlaceholder(c.AlertName, "no alert name"), c.Phase)

	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tSTATUS\tIN\tOUT\tUSD\tTOOLS\tWALL")
	for _, cp := range checkpoints {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.4f\t%d\t%s\n",
			cp.Phase, cp.Status, cp.Spend.InputTokens, cp.Spend.OutputTokens,
			cp.Spend.USD, cp.Spend.ToolCalls, wall(cp.Spend.WallMS))
	}
	fmt.Fprintf(tw, "TOTAL\t\t%d\t%d\t%.4f\t%d\t%s\n",
		total.InputTokens, total.OutputTokens, total.USD, total.ToolCalls, wall(total.WallMS))
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing cost table: %w", err)
	}
	return nil
}

// wall renders a millisecond duration for the cost table.
func wall(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

// orPlaceholder returns s, or fallback when s is empty.
func orPlaceholder(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
