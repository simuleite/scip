package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/urfave/cli/v2"
	rst "github.com/sourcegraph/scip/cmd/scip/rst"
	"google.golang.org/protobuf/proto"
)

func rstCLICommands() *cli.Command {
	tree := treeRepoCommand()
	structCmd := getFileStructureCommand()
	symCmd := getFileSymbolCommand()
	cmd := cli.Command{
		Name:  "cli",
		Usage: "CLI commands for RST-based code navigation",
		Description: `Provides CLI tools for navigating code using RST (Relation Symbol Table).
These commands are compatible with reni CLI interface.`,
		Subcommands: []*cli.Command{&tree, &structCmd, &symCmd},
	}
	return &cmd
}

func treeRepoCommand() cli.Command {
	var outputDir string
	command := cli.Command{
		Name:  "tree_repo",
		Usage: "List all files in the repository",
		Description: `Lists all files in the repository from RST index.
Example:
  scip cli tree_repo github.com/sourcegraph/scip`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "output",
				Usage:       "Directory containing RST files",
				Destination: &outputDir,
				Value:       "~/.rsts",
			},
		},
		Action: func(c *cli.Context) error {
			repo := c.Args().Get(0)
			if repo == "" {
				return errors.New("missing argument for repository name")
			}
			return treeRepoMain(outputDir, repo, c.App.Writer)
		},
	}
	return command
}

func treeRepoMain(outputDir, repo string, out io.Writer) error {
	// Expand ~ to home directory
	outputDir = expandHome(outputDir)

	// Convert repo name to RST file path
	rstFileName := strings.ReplaceAll(repo, ".", "_")
	rstFileName = strings.ReplaceAll(rstFileName, "/", "_")
	rstFileName += ".go.rst"
	rstPath := filepath.Join(outputDir, rstFileName)

	// Check if RST file exists
	if _, err := os.Stat(rstPath); err != nil {
		if os.IsNotExist(err) {
			return errors.Errorf("RST file not found for repo %s: %s", repo, rstPath)
		}
		return errors.Wrapf(err, "failed to stat %s", rstPath)
	}

	// Build file tree structure
	fileMap := make(map[string][]string)

	if err := addFilesToTree(rstPath, fileMap, make(map[string]bool)); err != nil {
		return errors.Wrapf(err, "failed to read %s", rstPath)
	}

	// Output in reni-compatible format
	fmt.Fprintf(out, `{"files":{`)
	first := true
	var dirs []string
	for dir := range fileMap {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		if !first {
			fmt.Fprint(out, ",")
		}
		fmt.Fprintf(out, "%q:[", dir)
		files := fileMap[dir]
		for i, f := range files {
			if i > 0 {
				fmt.Fprint(out, ",")
			}
			fmt.Fprintf(out, "%q", filepath.Base(f))
		}
		fmt.Fprint(out, "]")
		first = false
	}
	fmt.Fprintln(out, "}}")
	return nil
}

func addFilesToTree(rstPath string, fileMap map[string][]string, dirSet map[string]bool) error {
	data, err := os.ReadFile(rstPath)
	if err != nil {
		return err
	}

	var r rst.RST
	if err := proto.Unmarshal(data, &r); err != nil {
		return err
	}

	for path := range r.Documents {
		dir := filepath.Dir(path)
		if dir == "." {
			dir = ""
		}
		fileMap[dir] = append(fileMap[dir], path)
		dirSet[dir] = true
	}
	return nil
}

func getFileStructureCommand() cli.Command {
	var outputDir string
	command := cli.Command{
		Name:  "get_file_structure",
		Usage: "List all symbols in a file",
		Description: `Lists all symbols defined in a file from the RST index.
Example:
  scip cli get_file_structure github.com/sourcegraph/scip bindings/go/scip/assertions_noop.go`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "output",
				Usage:       "Directory containing RST files",
				Destination: &outputDir,
				Value:       "~/.rsts",
			},
		},
		Action: func(c *cli.Context) error {
			repo := c.Args().Get(0)
			filePath := c.Args().Get(1)
			if repo == "" {
				return errors.New("missing argument for repository name")
			}
			if filePath == "" {
				return errors.New("missing argument for file path")
			}
			return getFileStructureMain(outputDir, repo, filePath, c.App.Writer)
		},
	}
	return command
}

func getFileStructureMain(outputDir, repo, filePath string, out io.Writer) error {
	outputDir = expandHome(outputDir)

	// Convert repo name to RST file path
	rstFile := findRSTFileByRepo(outputDir, repo)
	if rstFile == "" {
		return errors.Errorf("file not found in any RST: %s", filePath)
	}

	// Read symbols from RST
	symbols, err := getSymbolsFromRST(rstFile, filePath)
	if err != nil {
		return err
	}

	// Output format
	fmt.Fprintf(out, `{"file_path":%q,"mod_path":%q,"pkg_path":%q,"nodes":[`, filePath, repo, extractPkgPath(repo))
	first := true
	for _, sym := range symbols {
		if !first {
			fmt.Fprint(out, ",")
		}
		fmt.Fprintf(out, `{"name":%q,"signature":%q,"line":%d}`, sym.Name, sym.Signature, sym.Line)
		first = false
	}
	fmt.Fprintln(out, "]}")
	return nil
}

func findRSTFile(outputDir, filePath string) string {
	entries, _ := os.ReadDir(outputDir)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rst") {
			rstPath := filepath.Join(outputDir, entry.Name())
			if containsFile(rstPath, filePath) {
				return rstPath
			}
		}
	}
	return ""
}

func findRSTFileByRepo(outputDir, repo string) string {
	rstFileName := strings.ReplaceAll(repo, ".", "_")
	rstFileName = strings.ReplaceAll(rstFileName, "/", "_")
	rstFileName += ".go.rst"
	rstPath := filepath.Join(outputDir, rstFileName)
	if _, err := os.Stat(rstPath); err == nil {
		return rstPath
	}
	return ""
}

func containsFile(rstPath, filePath string) bool {
	data, err := os.ReadFile(rstPath)
	if err != nil {
		return false
	}

	var r rst.RST
	if err := proto.Unmarshal(data, &r); err != nil {
		return false
	}

	_, ok := r.Documents[filePath]
	return ok
}

type SymbolInfo struct {
	Name      string
	Signature string
	Line      int32
}

func getSymbolsFromRST(rstPath, filePath string) ([]SymbolInfo, error) {
	data, err := os.ReadFile(rstPath)
	if err != nil {
		return nil, err
	}

	var r rst.RST
	if err := proto.Unmarshal(data, &r); err != nil {
		return nil, err
	}

	doc, ok := r.Documents[filePath]
	if !ok {
		return nil, errors.Errorf("file not found: %s", filePath)
	}

	var symbols []SymbolInfo
	for symKey, sym := range doc.Symbols {
		name := extractSymbolName(symKey)
		symbols = append(symbols, SymbolInfo{
			Name:      name,
			Signature: sym.Signature,
			Line:      sym.Line,
		})
	}
	return symbols, nil
}

func extractSymbolName(scipSymbol string) string {
	lastTick := strings.LastIndex(scipSymbol, "`")
	if lastTick == -1 {
		return scipSymbol
	}
	afterTick := scipSymbol[lastTick+1:]
	afterTick = strings.TrimPrefix(afterTick, "/")
	afterTick = strings.TrimSuffix(afterTick, "#")
	afterTick = strings.TrimSuffix(afterTick, ".")
	// Also remove trailing () for functions
	afterTick = strings.TrimSuffix(afterTick, "()")
	return afterTick
}

func extractPkgPath(repo string) string {
	// Convert repo to package path (e.g., github.com/sourcegraph/scip -> github.com/sourcegraph/scip)
	// For Go modules, the package path is typically repo + "/bindings/go/scip"
	return repo + "/bindings/go/scip"
}

func getFileSymbolCommand() cli.Command {
	var outputDir string
	command := cli.Command{
		Name:  "get_file_symbol",
		Usage: "Get symbol details including dependencies and references",
		Description: `Gets detailed information about a symbol including its
dependencies and references.
Example:
  scip cli get_file_symbol github.com/sourcegraph/scip bindings/go/scip/assertions_noop.go assert`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "output",
				Usage:       "Directory containing RST files",
				Destination: &outputDir,
				Value:       "~/.rsts",
			},
		},
		Action: func(c *cli.Context) error {
			repo := c.Args().Get(0)
			filePath := c.Args().Get(1)
			symbolName := c.Args().Get(2)
			if repo == "" {
				return errors.New("missing argument for repository name")
			}
			if filePath == "" {
				return errors.New("missing argument for file path")
			}
			if symbolName == "" {
				return errors.New("missing argument for symbol name")
			}
			return getFileSymbolMain(outputDir, repo, filePath, symbolName, c.App.Writer)
		},
	}
	return command
}

func getFileSymbolMain(outputDir, repo, filePath, symbolName string, out io.Writer) error {
	outputDir = expandHome(outputDir)

	// Convert repo name to RST file path
	rstFile := findRSTFileByRepo(outputDir, repo)
	if rstFile == "" {
		return errors.Errorf("file not found in any RST: %s", filePath)
	}

	// Get symbol details
	details, err := getSymbolDetails(rstFile, filePath, symbolName)
	if err != nil {
		return err
	}

	// Output in reni-compatible format
	fmt.Fprintf(out, `{"nodes":[`)
	fmt.Fprintf(out, `{"name":%q,"type":%q,"file":%q,"line":%d`, details.Name, details.Kind, filePath, details.Line)
	if len(details.Dependencies) > 0 {
		fmt.Fprintf(out, `,"dependencies":[{"file_path":%q,"names":[`, details.FilePath)
		for i, dep := range details.Dependencies {
			if i > 0 {
				fmt.Fprint(out, ",")
			}
			fmt.Fprintf(out, "%q", extractSymbolName(dep))
		}
		fmt.Fprint(out, "]}]")
	}
	if len(details.References) > 0 {
		fmt.Fprintf(out, `,"references":[{"file_path":%q,"names":[`, details.FilePath)
		for i, ref := range details.References {
			if i > 0 {
				fmt.Fprint(out, ",")
			}
			fmt.Fprintf(out, "%q", extractSymbolName(ref))
		}
		fmt.Fprint(out, "]}]")
	}
	fmt.Fprintln(out, "}]}")
	return nil
}

type SymbolDetails struct {
	Name         string
	Kind         string
	FilePath     string
	Line         int
	Dependencies []string
	References   []string
}

func getSymbolDetails(rstPath, filePath, symbolName string) (*SymbolDetails, error) {
	data, err := os.ReadFile(rstPath)
	if err != nil {
		return nil, err
	}

	var r rst.RST
	if err := proto.Unmarshal(data, &r); err != nil {
		return nil, err
	}

	doc, ok := r.Documents[filePath]
	if !ok {
		return nil, errors.Errorf("file not found: %s", filePath)
	}

	// Find matching symbol
	for symKey, sym := range doc.Symbols {
		baseName := extractSymbolName(symKey)
		if baseName == symbolName || strings.HasSuffix(baseName, "."+symbolName) {
			return &SymbolDetails{
				Name:         baseName,
				Kind:         sym.Kind,
				FilePath:     filePath,
				Line:         1,
				Dependencies: sym.DependenceOn,
				References:   sym.ReferenceBy,
			}, nil
		}
	}

	return nil, errors.Errorf("symbol not found: %s", symbolName)
}
