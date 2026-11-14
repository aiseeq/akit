// price converts an API price to a subscription-week percentage or subscription dollars.
package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/rates"
)

const commandName = "price"

var usage = fmt.Sprintf(`%s — convert an API dollar amount to subscription cost.

Usage:
  %s --usd AMOUNT --provider PROVIDER [--plan PLAN] [--unit week|usd] [--rates FILE]

Default unit week: percent of one week of a $100 subscription, using
AMOUNT / (api_usd_week × 100 / plan_usd_month) × 100. Without --plan the
latest row for the provider across all plans is used. Equal dates use the
last row in rates.tsv. The chosen plan and date are printed below the result;
when several plans share the latest date, a note names them.

The result has three significant digits, without an exponent.

Unit usd converts API dollars to subscription dollars with
AMOUNT × cents_per_api_usd / 100 × caution. The rates.tsv header defines
caution as the multiplier for the raw cents_per_api_usd rate; week percentages
use api_usd_week directly and do not use caution.

Rates file: $XDG_CONFIG_HOME/akit/rates.tsv, or ~/.config/akit/rates.tsv.
Example: %s --usd 10 --provider anthropic

Exit: 0 converted; 1 rates file/provider/plan/data error; 2 invalid invocation.
`, commandName, commandName, commandName)

const (
	exitData  = 1
	exitUsage = 2
)

var errHelp = errors.New("help")

type options struct {
	amount                          float64
	provider, plan, unit, ratesPath string
	amountSet, providerSet, unitSet bool
}

type env struct{ home, configHome string }

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n(%s -h for usage)\n", commandName, err, commandName)
		os.Exit(exitUsage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", commandName, err)
		os.Exit(exitData)
	}
	os.Exit(run(opts, env{home: home, configHome: os.Getenv("XDG_CONFIG_HOME")}, time.Now(), os.Stdout, os.Stderr))
}

func parseArgs(args []string) (options, error) {
	opts := options{unit: "week"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			return opts, errHelp
		}
		if !strings.HasPrefix(arg, "--") {
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			name = arg
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs a value", arg)
			}
			i++
			value = args[i]
		}
		if err := opts.set(name, value); err != nil {
			return opts, err
		}
	}
	if !opts.amountSet || !opts.providerSet {
		return opts, errors.New("--usd and --provider are required")
	}
	return opts, nil
}

func (opts *options) set(name, value string) error {
	switch name {
	case "--usd":
		if opts.amountSet {
			return errors.New("--usd supplied more than once")
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("--usd must be a positive number, got %q", value)
		}
		opts.amount, opts.amountSet = n, true
	case "--provider":
		if opts.providerSet || value == "" {
			return errors.New("--provider must be supplied once with a value")
		}
		opts.provider, opts.providerSet = value, true
	case "--plan":
		if opts.plan != "" || value == "" {
			return errors.New("--plan must be supplied at most once with a value")
		}
		opts.plan = value
	case "--unit":
		if opts.unitSet {
			return errors.New("--unit supplied more than once")
		}
		if value != "week" && value != "usd" {
			return errors.New("--unit must be week or usd")
		}
		opts.unit, opts.unitSet = value, true
	case "--rates":
		if opts.ratesPath != "" || value == "" {
			return errors.New("--rates must be supplied at most once with a file")
		}
		opts.ratesPath = value
	default:
		return fmt.Errorf("unknown flag %q", name)
	}
	return nil
}

func run(opts options, e env, now time.Time, out, errOut io.Writer) int {
	path := opts.ratesPath
	if path == "" {
		base := e.configHome
		if base == "" {
			base = filepath.Join(e.home, ".config")
		}
		path = filepath.Join(base, "akit", "rates.tsv")
	}
	table, err := rates.Read(path, now)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", commandName, err)
		return exitData
	}
	row, ok := table.Latest(opts.provider, opts.plan)
	if !ok {
		if opts.plan != "" {
			fmt.Fprintf(errOut, "%s: no rate for provider %q and plan %q in %s\n", commandName, opts.provider, opts.plan, path)
		} else {
			fmt.Fprintf(errOut, "%s: no rate for provider %q in %s\n", commandName, opts.provider, path)
		}
		return exitData
	}
	if row.InvalidPlan != nil {
		fmt.Fprintf(errOut, "%s: %v\n", commandName, row.InvalidPlan)
		return exitData
	}
	var result float64
	switch opts.unit {
	case "week":
		if row.InvalidWeek != nil {
			fmt.Fprintf(errOut, "%s: %v\n", commandName, row.InvalidWeek)
			return exitData
		}
		result = opts.amount / (row.APIUSDWeek * 100 / row.PlanUSDMonth) * 100
	case "usd":
		if row.InvalidCents != nil {
			fmt.Fprintf(errOut, "%s: %v\n", commandName, row.InvalidCents)
			return exitData
		}
		if row.InvalidCaution != nil {
			fmt.Fprintf(errOut, "%s: %v\n", commandName, row.InvalidCaution)
			return exitData
		}
		if row.CentsPerAPIUSD == 0 || row.Caution == 0 {
			fmt.Fprintf(errOut, "%s: %s:%d: cents_per_api_usd and caution are required for --unit usd\n", commandName, path, row.Line)
			return exitData
		}
		result = opts.amount * row.CentsPerAPIUSD / 100 * row.Caution
	}
	plan := row.Plan
	if plan == "" {
		plan = "(unspecified)"
	}
	amount := "$" + strconv.FormatFloat(opts.amount, 'f', -1, 64)
	formattedResult := significant(result)
	if opts.unit == "usd" {
		fmt.Fprintf(out, "%s API = $%s subscription\n", amount, formattedResult)
	} else {
		fmt.Fprintf(out, "%s API = %s%% of one $100 %s subscription week\n", amount, formattedResult, opts.provider)
	}
	fmt.Fprintf(out, "rate: provider=%s plan=%s valid_from=%s (%s:%d)\n", opts.provider, plan, row.ValidFrom.Format(time.DateOnly), path, row.Line)
	if opts.plan == "" {
		plans := table.PlansAtLatest(opts.provider)
		if len(plans) > 1 {
			for i, p := range plans {
				if p == "" {
					plans[i] = "(unspecified)"
				}
			}
			fmt.Fprintf(out, "note: multiple plans on %s: %s; use --plan to select one\n", row.ValidFrom.Format(time.DateOnly), strings.Join(plans, ","))
		}
	}
	return 0
}

// significant prints three significant digits without an exponent: a run
// costs hundredths of a percent, a week of work hundreds.
func significant(v float64) string {
	if v == 0 {
		return "0"
	}
	exp := math.Floor(math.Log10(math.Abs(v)))
	rounded := 0.0
	if exp >= 2 {
		unit := math.Pow(10, exp-2)
		rounded = math.Round(v/unit) * unit
	} else {
		scale := math.Pow(10, 2-exp)
		rounded = math.Round(v*scale) / scale
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}
