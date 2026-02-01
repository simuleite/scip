package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/cockroachdb/errors"
	"github.com/k0kubun/pp/v3"
	"github.com/urfave/cli/v2"
	rst "github.com/sourcegraph/scip/cmd/scip/rst"
	"google.golang.org/protobuf/proto"
)

func printCommand() cli.Command {
	var json, colorEnabled bool
	snapshot := cli.Command{
		Name:  "print",
		Usage: "Print a SCIP index or RST file for debugging",
		Description: `Prints a SCIP index (.scip) or RST file (.rst) in readable format.
WARNING: The TTY output may change over time.
Do not rely on non-JSON output in scripts`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "json",
				Usage:       "Output in JSON format",
				Destination: &json,
			},
			&cli.BoolFlag{
				Name:        "color",
				Usage:       "Enable color output for TTY (no effect for JSON)",
				Destination: &colorEnabled,
				Value:       true,
				DefaultText: "true",
			},
		},
		Action: func(c *cli.Context) error {
			indexPath := c.Args().Get(0)
			if indexPath == "" {
				return errors.New("missing argument for path to file")
			}
			// Following https://no-color.org/
			if val, found := os.LookupEnv("NO_COLOR"); found && val != "" {
				switch strings.ToLower(val) {
				case "":
					break
				case "0", "false", "off":
					colorEnabled = false
				default:
					colorEnabled = true
				}
			}
			return printMain(indexPath, colorEnabled, json, c.App.Writer)
		},
	}
	return snapshot
}

func printMain(indexPath string, colorEnabled bool, json bool, out io.Writer) error {
	ext := strings.ToLower(filepath.Ext(indexPath))

	if ext == ".rst" {
		return printRST(indexPath, colorEnabled, json, out)
	}
	return printSCIP(indexPath, colorEnabled, json, out)
}

func printRST(indexPath string, colorEnabled bool, json bool, out io.Writer) error {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return errors.Wrapf(err, "failed to read %s", indexPath)
	}

	var r rst.RST
	if err := proto.Unmarshal(data, &r); err != nil {
		return errors.Wrapf(err, "failed to unmarshal RST from %s", indexPath)
	}

	if json {
		encoder := sonic.ConfigDefault.NewEncoder(out)
		return encoder.Encode(&r)
	}
	prettyPrinter := pp.New()
	prettyPrinter.SetColoringEnabled(colorEnabled)
	prettyPrinter.SetExportedOnly(true)
	prettyPrinter.SetOutput(out)
	_, err = prettyPrinter.Print(&r)
	return err
}

func printSCIP(indexPath string, colorEnabled bool, json bool, out io.Writer) error {
	index, err := readFromOption(indexPath)
	if err != nil {
		return err
	}
	if json {
		encoder := sonic.ConfigDefault.NewEncoder(out)
		return encoder.Encode(index)
	}
	prettyPrinter := pp.New()
	prettyPrinter.SetColoringEnabled(colorEnabled)
	prettyPrinter.SetExportedOnly(true)
	prettyPrinter.SetOutput(out)
	_, err = prettyPrinter.Print(index)
	return err
}
