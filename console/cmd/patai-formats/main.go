// patai-formats keeps a release off data it can't use (docs/updates.md, Data across slots).
//
//	patai-formats declared [<recipes>]           what a release's services declare of their data, as JSON:
//	                                               data-formats.json, which scripts/build-product writes
//	patai-formats install-check <formats.json>   on a unit, before it installs the release that declares these: says
//	                                               why not and exits 10. bundle/hook runs the one in the bundle
//	patai-formats start <service>                on a unit: says why this release can't start the service on what
//	                                               /data holds, and exits 1. patai-workload runs it
//
// --recipes is the running release's recipes, /usr/share/patai/workloads, and --data is /data.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"patai/console/internal/formats"
	"patai/console/internal/workloads"
)

const usage = "usage: patai-formats [--recipes <dir>] [--data <dir>] declared [<recipes>] | install-check <formats.json> | start <service>"

func die(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(code)
}

func main() {
	recipes := flag.String("recipes", workloads.Recipes, "the running release's recipes")
	data := flag.String("data", "/data", "where the apps keep their data")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		die(2, usage)
	}
	switch {
	case args[0] == "declared" && len(args) <= 2:
		dir := *recipes
		if len(args) == 2 {
			dir = args[1]
		}
		release, err := workloads.DataFormats(dir)
		if err != nil {
			die(1, "%v", err)
		}
		out, _ := json.MarshalIndent(release, "", "  ")
		fmt.Println(string(out))
	case args[0] == "install-check" && len(args) == 2:
		raw, err := os.ReadFile(args[1])
		if err != nil {
			die(1, "%v", err)
		}
		var next formats.Release
		if err := json.Unmarshal(raw, &next); err != nil {
			die(1, "%s: %v", args[1], err)
		}
		if err := next.Check(); err != nil {
			die(10, "the bundle declares %v", err)
		}
		running, err := workloads.DataFormats(*recipes)
		if err != nil {
			die(1, "%v", err)
		}
		unit, err := formats.ReadUnit(*data, running, next)
		if err != nil {
			die(1, "%v", err)
		}
		if refused := formats.Install(unit, running, next); len(refused) > 0 {
			for _, r := range refused {
				fmt.Fprintln(os.Stderr, r.Error())
			}
			os.Exit(10)
		}
	case args[0] == "start" && len(args) == 2:
		running, err := workloads.DataFormats(*recipes)
		if err != nil {
			die(1, "%v", err)
		}
		if _, ok := running[args[1]]; !ok {
			return
		}
		unit, err := formats.ReadUnit(*data, formats.Release{args[1]: running[args[1]]})
		if err != nil {
			die(1, "%v", err)
		}
		if r := formats.Start(args[1], unit[args[1]], running); r != nil {
			die(1, "%s (%s)", upper(r.Reason), r.Detail)
		}
	default:
		die(2, usage)
	}
}

// upper starts a sentence with a capital, as patai-workload says it.
func upper(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
