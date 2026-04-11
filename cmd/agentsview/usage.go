package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/pricing"
)

func runUsage(args []string) {
	if len(args) == 0 {
		printUsageHelp()
		os.Exit(1)
	}

	switch args[0] {
	case "daily":
		runUsageDaily(args[1:])
	case "statusline":
		runUsageStatusline(args[1:])
	case "help", "--help", "-h":
		printUsageHelp()
	default:
		fmt.Fprintf(os.Stderr,
			"unknown usage subcommand: %s\n", args[0])
		printUsageHelp()
		os.Exit(1)
	}
}

func runUsageDaily(args []string) {
	fs := flag.NewFlagSet("usage daily", flag.ExitOnError)
	jsonOut := fs.Bool("json", false,
		"Output as JSON")
	since := fs.String("since", "",
		"Start date (YYYY-MM-DD)")
	until := fs.String("until", "",
		"End date (YYYY-MM-DD)")
	agent := fs.String("agent", "",
		"Filter by agent (claude, codex)")
	breakdown := fs.Bool("breakdown", false,
		"Show per-model breakdown rows")
	offline := fs.Bool("offline", false,
		"Use fallback pricing only")
	timezone := fs.String("timezone", "",
		"IANA timezone for date bucketing")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	database := openUsageDB()
	defer database.Close()

	ensurePricing(database, *offline)

	filter := db.UsageFilter{
		From:     *since,
		To:       *until,
		Agent:    *agent,
		Timezone: *timezone,
	}

	result, err := database.GetDailyUsage(
		context.Background(), filter,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printDailyTable(result, *breakdown)
}

func runUsageStatusline(args []string) {
	fs := flag.NewFlagSet("usage statusline", flag.ExitOnError)
	agent := fs.String("agent", "",
		"Filter by agent (claude, codex)")
	offline := fs.Bool("offline", false,
		"Use fallback pricing only")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	database := openUsageDB()
	defer database.Close()

	ensurePricing(database, *offline)

	today := time.Now().Format("2006-01-02")
	filter := db.UsageFilter{
		From:  today,
		To:    today,
		Agent: *agent,
	}

	result, err := database.GetDailyUsage(
		context.Background(), filter,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *agent != "" {
		fmt.Printf("$%.2f today (%s)\n",
			result.Totals.TotalCost, *agent)
	} else {
		fmt.Printf("$%.2f today\n", result.Totals.TotalCost)
	}
}

func openUsageDB() *db.DB {
	cfg, err := config.LoadMinimal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"error opening database: %v\n", err)
		os.Exit(1)
	}
	return database
}

func ensurePricing(database *db.DB, offline bool) {
	var prices []pricing.ModelPricing

	if offline {
		prices = pricing.FallbackPricing()
	} else {
		var err error
		prices, err = pricing.FetchLiteLLMPricing()
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing fetch failed: %v"+
					"; using fallback\n", err)
			prices = pricing.FallbackPricing()
		}
	}

	dbPrices := make([]db.ModelPricing, len(prices))
	for i, p := range prices {
		dbPrices[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
		}
	}

	if err := database.UpsertModelPricing(dbPrices); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: could not update pricing: %v\n", err)
	}
}

func printDailyTable(
	result db.DailyUsageResult, breakdown bool,
) {
	w := tabwriter.NewWriter(
		os.Stdout, 0, 4, 2, ' ', 0,
	)

	fmt.Fprintln(w,
		"DATE\tINPUT\tOUTPUT\tCACHE_CR\tCACHE_RD\tCOST\tMODELS")
	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")

	for _, day := range result.Daily {
		models := joinModels(day.ModelsUsed)
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t$%.4f\t%s\n",
			day.Date,
			day.InputTokens,
			day.OutputTokens,
			day.CacheCreationTokens,
			day.CacheReadTokens,
			day.TotalCost,
			models,
		)

		if breakdown {
			for _, mb := range day.ModelBreakdowns {
				fmt.Fprintf(w,
					"  %s\t%d\t%d\t%d\t%d\t$%.4f\t\n",
					mb.ModelName,
					mb.InputTokens,
					mb.OutputTokens,
					mb.CacheCreationTokens,
					mb.CacheReadTokens,
					mb.Cost,
				)
			}
		}
	}

	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t$%.4f\t\n",
		result.Totals.InputTokens,
		result.Totals.OutputTokens,
		result.Totals.CacheCreationTokens,
		result.Totals.CacheReadTokens,
		result.Totals.TotalCost,
	)

	w.Flush()
}

func joinModels(models []string) string {
	if len(models) == 0 {
		return ""
	}
	var s strings.Builder
	s.WriteString(models[0])
	for _, m := range models[1:] {
		s.WriteString(", " + m)
	}
	return s.String()
}

func printUsageHelp() {
	fmt.Fprint(os.Stderr, `Usage: agentsview usage <command> [flags]

Commands:
  daily       Daily cost summary
  statusline  One-line cost summary for today
  help        Show this help

Daily flags:
  --json              Output as JSON
  --since YYYY-MM-DD  Start date
  --until YYYY-MM-DD  End date
  --agent string      Filter by agent (claude, codex)
  --breakdown         Show per-model breakdown rows
  --offline           Use fallback pricing only
  --timezone string   IANA timezone for date bucketing

Statusline flags:
  --agent string      Filter by agent (claude, codex)
  --offline           Use fallback pricing only
`)
}
