package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/urfave/cli/v2"
	"google.golang.org/protobuf/proto"

	"github.com/sourcegraph/scip/bindings/go/scip"
	rst "github.com/sourcegraph/scip/cmd/scip/rst"
)

func parseCommand() cli.Command {
	var outputDir, repoID string
	command := cli.Command{
		Name:  "parse",
		Usage: "Parse SCIP index to RST (Relation Symbol Table) format",
		Description: `Converts a SCIP index to RST format for efficient code navigation.
Example:
  scip parse index.scip -o ~/.rsts --repo github.com/sourcegraph/scip

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
				Usage:       "Repository identifier (e.g., github.com/sourcegraph/scip)",
				Destination: &repoID,
				Required:    true,
			},
		},
		Action: func(c *cli.Context) error {
			indexPath := c.Args().Get(0)
			if indexPath == "" {
				return errors.New("missing argument for path to SCIP index")
			}
			return parseMain(indexPath, outputDir, repoID)
		},
	}
	return command
}

func parseMain(indexPath, outputDir, repoID string) error {
	index, err := readFromOption(indexPath)
	if err != nil {
		return err
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
		rstTable := buildRST(docs, repoID)

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

func buildRST(docs []*scip.Document, repoID string) *rst.RST {
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

		for _, sym := range doc.Symbols {
			if scip.IsLocalSymbol(sym.Symbol) {
				continue
			}
			rstSym := &rst.Symbol{
				Symbol: sym.Symbol,
				Kind:   sym.Kind.String(),
			}
			if len(sym.Documentation) > 0 {
				rstSym.Documentation = strings.Join(sym.Documentation, "\n")
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
