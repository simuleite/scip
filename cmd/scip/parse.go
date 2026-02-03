package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/urfave/cli/v2"
	"google.golang.org/protobuf/proto"

	"github.com/sourcegraph/scip/bindings/go/scip"
	rst "github.com/sourcegraph/scip/cmd/scip/rst"
)

// codeBlockNodeTypes maps language SCIP codes to tree-sitter node types for extractable code blocks
// Includes functions, methods, structs, classes, interfaces, types, etc.
var codeBlockNodeTypes = map[string][]string{
	"go": {
		"function_declaration", "method_declaration",
		"type_declaration", "type_spec",
	},
	"typescript": {
		"function_declaration", "function_expression", "arrow_function", "method_definition", "generator_function",
		"class_declaration", "interface_declaration", "type_alias_declaration",
	},
	"javascript": {
		"function_declaration", "function_expression", "arrow_function", "method_definition", "generator_function",
		"class_declaration",
	},
	"python": {
		"function_definition",
		"class_definition",
	},
	"rust": {
		"function_item",
		"struct_item", "enum_item", "impl_item", "trait_item",
	},
	"java": {
		"method_declaration", "constructor_declaration",
		"class_declaration", "interface_declaration", "enum_declaration",
	},
	"c": {
		"function_definition", "method_definition",
		"struct_specifier", "enum_specifier",
	},
	"cpp": {
		"function_definition", "method_definition",
		"struct_specifier", "class_specifier", "enum_specifier",
	},
}

// detectRepoID extracts the module name from SCIP symbol format.
// SCIP symbol format: "<tool> <manager> <module> <commit> `path`/symbol"
func detectRepoID(index *scip.Index) string {
	for _, doc := range index.Documents {
		for _, sym := range doc.Symbols {
			if sym.Symbol == "" || scip.IsLocalSymbol(sym.Symbol) {
				continue
			}
			// Parse SCIP symbol format
			parts := strings.SplitN(sym.Symbol, " ", 4)
			if len(parts) >= 3 {
				// parts[2] is the module name (e.g., "github.com/sourcegraph/scip")
				return parts[2]
			}
		}
	}
	return ""
}

func parseCommand() cli.Command {
	var outputDir, repoID string
	var verbose bool
	command := cli.Command{
		Name:  "parse",
		Usage: "Parse SCIP index to RST (Relation Symbol Table) format",
		Description: `Converts a SCIP index to RST format for efficient code navigation.
Example:
  scip parse index.scip -o ~/.rsts

Output files are stored as protobuf binary format:
  {Sanitized_Repo_ID}.{Language_Code}.rst

Use 'scip print' to output RST as JSON for debugging.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "output",
				Usage:       "Directory to output RST files",
				Destination: &outputDir,
				Value:       "~/.rsts",
			},
			&cli.StringFlag{
				Name:        "repo",
				Usage:       "Repository identifier (auto-detected from SCIP index if not specified)",
				Destination: &repoID,
			},
			&cli.BoolFlag{
				Name:        "verbose",
				Usage:       "Enable verbose debug output",
				Destination: &verbose,
			},
		},
		Action: func(c *cli.Context) error {
			indexPath := c.Args().Get(0)
			if indexPath == "" {
				return errors.New("missing argument for path to SCIP index")
			}
			return parseMain(indexPath, outputDir, repoID, verbose)
		},
	}
	return command
}

func parseMain(indexPath, outputDir, repoID string, verbose bool) error {
	index, err := readFromOption(indexPath)
	if err != nil {
		return err
	}

	if verbose {
		println("DEBUG: parseMain called")
	}

	// Auto-detect repoID from SCIP index if not provided
	if repoID == "" {
		repoID = detectRepoID(index)
		if repoID == "" {
			return errors.New("could not auto-detect repo ID; please specify --repo")
		}
	}

	// Extract project root from index metadata
	var projectRoot string
	if verbose {
		fmt.Fprintf(os.Stderr, "DEBUG: index=%p, index.Metadata=%p\n", index, index.Metadata)
	}
	if index.Metadata != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "DEBUG: ProjectRoot=%q\n", index.Metadata.ProjectRoot)
		}
		if index.Metadata.ProjectRoot != "" {
			projectRoot = stripFilePrefix(index.Metadata.ProjectRoot)
		}
	}

	// Expand ~ to home directory
	outputDir = expandHome(outputDir)

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return errors.Wrapf(err, "failed to create output directory %s", outputDir)
	}

	// Group documents by language
	langDocs := make(map[string][]*scip.Document)
	for _, doc := range index.Documents {
		lang := doc.Language
		if lang == "" {
			lang = "unknown"
		}
		langDocs[lang] = append(langDocs[lang], doc)
	}

	// Process each language
	for lang, docs := range langDocs {
		rstTable := buildRST(docs, repoID, projectRoot, verbose)

		// Sanitize repo ID for filename
		sanitizedRepoID := sanitizeRepoID(repoID)
		filename := fmt.Sprintf("%s.%s.rst", sanitizedRepoID, lang)
		outputPath := filepath.Join(outputDir, filename)

		// Write with atomic rename (tmp -> rst)
		tmpPath := outputPath + ".tmp"
		if err := writeRST(tmpPath, rstTable); err != nil {
			return errors.Wrapf(err, "failed to write RST to %s", tmpPath)
		}
		if err := os.Rename(tmpPath, outputPath); err != nil {
			return errors.Wrapf(err, "failed to rename %s to %s", tmpPath, outputPath)
		}
		fmt.Printf("Generated RST: %s\n", outputPath)
	}

	return nil
}

func buildRST(docs []*scip.Document, repoID, projectRoot string, verbose bool) *rst.RST {
	rstTable := &rst.RST{
		Metadata: &rst.Metadata{
			Repo:     repoID,
			Language: docs[0].Language,
		},
		Documents: make(map[string]*rst.Document),
	}

	// Build symbol index for quick lookup
	symbolIndex := make(map[string]*rst.Symbol)
	for _, doc := range docs {
		rstDoc := &rst.Document{
			RelativePath: doc.RelativePath,
			Symbols:      make(map[string]*rst.Symbol),
		}

		// Build occurrence index for line number and kind lookup
		occIndex := make(map[string]struct {
			line int32
			kind string
		})
		for _, occ := range doc.Occurrences {
			if len(occ.Range) > 0 && occ.Symbol != "" {
				// Store the first (definition) occurrence's line
				if _, exists := occIndex[occ.Symbol]; !exists {
					// Determine kind from symbol_roles
					kind := inferKindFromRoles(occ.SymbolRoles)
					occIndex[occ.Symbol] = struct {
						line int32
						kind string
					}{
						line: int32(occ.Range[0]) + 1, // 1-indexed line
						kind: kind,
					}
				}
			}
		}

		for _, sym := range doc.Symbols {
			if scip.IsLocalSymbol(sym.Symbol) {
				continue
			}
			rstSym := &rst.Symbol{
				Symbol:    sym.Symbol,
				Kind:      sym.Kind.String(),
				Signature: buildSignature(sym),
			}
			// Set line number and kind from occurrence
			if info, ok := occIndex[sym.Symbol]; ok {
				rstSym.Line = info.line
				if rstSym.Kind == "UnspecifiedKind" || rstSym.Kind == "" {
					rstSym.Kind = info.kind
				}
			}
			// Extract source code from file using tree-sitter (only for functions/methods)
			if projectRoot != "" && rstSym.Line > 0 && doc.RelativePath != "" && isCodeExtractableKind(rstSym.Kind) {
				// Use RelativePath directly if it's already absolute, otherwise join with projectRoot
				sourceFile := doc.RelativePath
				if !filepath.IsAbs(doc.RelativePath) {
					sourceFile = filepath.Join(projectRoot, doc.RelativePath)
				}
				if verbose {
					fmt.Fprintf(os.Stderr, "DEBUG: doc.RelativePath=%q, projectRoot=%q, isAbs=%v, sourceFile=%q\n",
						doc.RelativePath, projectRoot, filepath.IsAbs(doc.RelativePath), sourceFile)
				}
				code := treeSitterExtractCode(sourceFile, rstSym.Line, doc.Language, verbose)
				if code != "" {
					rstSym.Code = code
				}
			}
			rstDoc.Symbols[sym.Symbol] = rstSym
			symbolIndex[sym.Symbol] = rstSym
		}

		rstTable.Documents[doc.RelativePath] = rstDoc
	}

	// Build reference_by and dependence_on using enclosing_range
	for _, doc := range docs {
		for _, occ := range doc.Occurrences {
			if scip.IsLocalSymbol(occ.Symbol) {
				continue
			}
			if occ.EnclosingRange == nil || len(occ.EnclosingRange) < 3 {
				continue
			}

			// Get the enclosing range lines
			startLine := occ.EnclosingRange[0]
			endLine := occ.EnclosingRange[2]

			// Find other occurrences within this enclosing range
			for _, otherOcc := range doc.Occurrences {
				if otherOcc.Symbol == occ.Symbol {
					continue
				}
				if len(otherOcc.Range) == 0 {
					continue
				}
				otherLine := otherOcc.Range[0]
				if otherLine >= startLine && otherLine <= endLine {
					// This is a reference within the enclosing range
					if rstSym, ok := symbolIndex[occ.Symbol]; ok {
						if !scip.IsLocalSymbol(otherOcc.Symbol) {
							// Add to dependence_on
							found := false
							for _, dep := range rstSym.DependenceOn {
								if dep == otherOcc.Symbol {
									found = true
									break
								}
							}
							if !found {
								rstSym.DependenceOn = append(rstSym.DependenceOn, otherOcc.Symbol)
							}
						}
					}
					// Add to reference_by of the referenced symbol
					if otherRstSym, ok := symbolIndex[otherOcc.Symbol]; ok {
						found := false
						for _, ref := range otherRstSym.ReferenceBy {
							if ref == occ.Symbol {
								found = true
								break
							}
						}
						if !found {
							otherRstSym.ReferenceBy = append(otherRstSym.ReferenceBy, occ.Symbol)
						}
					}
				}
			}
		}
	}

	return rstTable
}

func sanitizeRepoID(repoID string) string {
	// Remove protocol prefixes
	repoID = strings.TrimPrefix(repoID, "https://")
	repoID = strings.TrimPrefix(repoID, "http://")
	repoID = strings.TrimPrefix(repoID, "git@")

	// Replace special characters with underscores
	result := strings.ReplaceAll(repoID, "/", "_")
	result = strings.ReplaceAll(result, ".", "_")
	result = strings.ReplaceAll(result, ":", "_")
	result = strings.ReplaceAll(result, "-", "_")

	return result
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func buildSignature(sym *scip.SymbolInformation) string {
	// Use signature documentation if available
	if sym.SignatureDocumentation != nil && sym.SignatureDocumentation.Text != "" {
		return sym.SignatureDocumentation.Text
	}

	// Try to extract signature from documentation (code block)
	for _, doc := range sym.Documentation {
		if strings.HasPrefix(doc, "```go\n") && strings.HasSuffix(doc, "\n```") {
			// Extract content between code block markers
			content := doc[6 : len(doc)-4]
			return content
		}
	}

	// Build a basic signature from display name and kind
	prefix := ""
	switch sym.Kind {
	case scip.SymbolInformation_Function, scip.SymbolInformation_Method:
		prefix = "func "
	case scip.SymbolInformation_Struct:
		prefix = "type "
	case scip.SymbolInformation_Class:
		prefix = "class "
	case scip.SymbolInformation_Interface:
		prefix = "interface "
	case scip.SymbolInformation_Constant:
		prefix = "const "
	case scip.SymbolInformation_Field:
		prefix = ""
	case scip.SymbolInformation_TypeParameter:
		prefix = ""
	}

	name := sym.DisplayName
	if name == "" {
		name = extractSymbolName(sym.Symbol)
	}
	return prefix + name
}

func inferKindFromRoles(roles int32) string {
	// Check for definition role
	isDefinition := (roles & int32(scip.SymbolRole_Definition)) > 0
	isForwardDefinition := (roles & int32(scip.SymbolRole_ForwardDefinition)) > 0

	if isDefinition || isForwardDefinition {
		return "FUNC"
	}
	return ""
}

func isCodeExtractableKind(kind string) bool {
	// Only extract code for functions, methods, and similar executable code blocks
	switch kind {
	case "Function", "Method", "FUNC", "Constructor", "Destructor":
		return true
	default:
		return false
	}
}

func writeRST(path string, rstTable *rst.RST) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write as protobuf (binary format)
	data, err := proto.Marshal(rstTable)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	return f.Sync()
}

// treeSitterExtractCode extracts the complete function body from a source file
// using tree-sitter to parse source code and locate the function containing the target line.
func treeSitterExtractCode(sourceFile string, line int32, lang string, verbose bool) string {
	if verbose {
		fmt.Fprintf(os.Stderr, "DEBUG: treeSitterExtractCode called with sourceFile=%q, line=%d, lang=%q\n", sourceFile, line, lang)
	}
	data, err := os.ReadFile(sourceFile)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "DEBUG: failed to read file: %v\n", err)
		}
		return ""
	}
	sourceCode := string(data)
	if verbose {
		fmt.Fprintf(os.Stderr, "DEBUG: read %d bytes from file\n", len(data))
	}

	// Parse source code with appropriate language
	var root *sitter.Node
	switch lang {
	case "go":
		root = sitter.Parse([]byte(sourceCode), golang.GetLanguage())
	default:
		// Fallback to Go parser for unknown languages
		root = sitter.Parse([]byte(sourceCode), golang.GetLanguage())
	}
	if root == nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "DEBUG: tree-sitter parse returned nil\n")
		}
		return ""
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "DEBUG: tree-sitter parse succeeded, root type=%s\n", root.Type())
	}

	// Get function/method node types for the language
	nodeTypes, ok := codeBlockNodeTypes[lang]
	if !ok {
		// Default to Go for unknown languages
		nodeTypes = codeBlockNodeTypes["go"]
	}

	// Build a set for quick lookup
	nodeTypeSet := make(map[string]bool)
	for _, t := range nodeTypes {
		nodeTypeSet[t] = true
	}

	// Find function containing the target line
	targetLine := uint32(line)
	var codeNode *sitter.Node
	dfsWalk(root, func(n *sitter.Node) {
		if nodeTypeSet[n.Type()] {
			start := n.StartPoint().Row + 1 // 1-based
			end := n.EndPoint().Row + 1
			if verbose {
				fmt.Fprintf(os.Stderr, "DEBUG: found %s at lines %d-%d\n", n.Type(), start, end)
			}
			if targetLine >= start && targetLine <= end {
				codeNode = n
				if verbose {
					fmt.Fprintf(os.Stderr, "DEBUG: matched %s at lines %d-%d for target line %d\n", n.Type(), start, end, targetLine)
				}
			}
		}
	})

	if codeNode == nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "DEBUG: no function found containing line %d\n", targetLine)
		}
		return ""
	}

	// Extract function source code
	start := codeNode.StartByte()
	end := codeNode.EndByte()
	code := sourceCode[start:end]
	if verbose {
		fmt.Fprintf(os.Stderr, "DEBUG: extracted %d bytes of code (start=%d, end=%d)\n", len(code), start, end)
	}
	return code
}

func dfsWalk(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		dfsWalk(node.Child(i), fn)
	}
}

// extractPathFromSymbol extracts the source file path from a SCIP symbol.
// SCIP symbol format: "scip-go gomod github.com/sourcegraph/scip c081503f250d `github.com/sourcegraph/scip/cmd/scip`/getFileSymbolMain()"
func extractPathFromSymbol(scipSymbol string) string {
	// Find backtick-enclosed path
	backtickStart := strings.Index(scipSymbol, "`")
	if backtickStart == -1 {
		return ""
	}
	backtickEnd := strings.Index(scipSymbol[backtickStart+1:], "`")
	if backtickEnd == -1 {
		return ""
	}
	pathWithModule := scipSymbol[backtickStart+1 : backtickStart+1+backtickEnd]

	// Extract path after the module prefix
	// Format: "github.com/sourcegraph/scip/cmd/scip" or "github.com/sourcegraph/scip/cmd/scip/rst.go"
	modulePrefix := "github.com/sourcegraph/scip/"
	idx := strings.Index(pathWithModule, modulePrefix)
	if idx == -1 {
		return pathWithModule
	}
	relativePath := pathWithModule[idx+len(modulePrefix):]

	// Add .go extension if not present
	if !strings.HasSuffix(relativePath, ".go") {
		relativePath += ".go"
	}
	return relativePath
}

// stripFilePrefix removes "file://" prefix from a file URL.
func stripFilePrefix(url string) string {
	// Handle file:///, file://, file:/ prefixes
	// file:///Users/... -> /Users/... (file:// + /Users/...)
	// file://localhost/Users/... -> /Users/...
	if strings.HasPrefix(url, "file:///") {
		return url[7:] // Keep the leading /
	}
	if strings.HasPrefix(url, "file://localhost/") {
		return url[16:]
	}
	if strings.HasPrefix(url, "file://") {
		return url[7:]
	}
	if strings.HasPrefix(url, "file:/") {
		return url[6:]
	}
	return url
}
