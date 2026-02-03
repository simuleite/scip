package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/urfave/cli/v2"
	rst "github.com/sourcegraph/scip/cmd/scip/rst"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/proto"
)

const rstDefaultPath = "~/.rsts"

var (
	columnStyle = lipgloss.NewStyle().
			BorderForeground(lipgloss.Color("240")).
			BorderStyle(lipgloss.NormalBorder()).
			Padding(0, 1)

	activeColumnStyle = lipgloss.NewStyle().
				BorderForeground(lipgloss.Color("63")).
				BorderStyle(lipgloss.NormalBorder()).
				Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5")).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	depStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6"))

	refStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))
)

// View modes
const (
	modeThreePane = iota
	modeSymbol
)

// Three-pane TUI model
type model struct {
	repos       list.Model
	files       list.Model
	symbols     list.Model
	deps        list.Model
	refs        list.Model
	viewport    viewport.Model
	mode        int
	active      int // 0: repos, 1: files, 2: symbols
	width       int
	height      int
	rstPath     string
	symbol      *symbolDetail
	symbolStack []symbolJump
}

type symbolJump struct {
	name      string
	signature string
	line      int
	filePath  string
	deps      []string
	refs      []string
	code      string
}

// List items
type repoItem struct{ name string }
type fileItem struct{ name string }
type symbolItem struct {
	name      string
	signature string
	line      int
}
type refItem struct {
	name string
	kind string // "dep" or "ref"
}

func (i repoItem) Title() string       { return i.name }
func (i repoItem) Description() string { return "" }
func (i repoItem) FilterValue() string { return i.name }

func (i fileItem) Title() string       { return i.name }
func (i fileItem) Description() string { return "" }
func (i fileItem) FilterValue() string { return i.name }

func (i symbolItem) Title() string       { return i.name }
func (i symbolItem) Description() string { return fmt.Sprintf("%s (line %d)", i.signature, i.line) }
func (i symbolItem) FilterValue() string { return i.name }

func (i refItem) Title() string       { return i.name }
func (i refItem) Description() string {
	if i.kind == "dep" {
		return "dependency"
	}
	return "reference"
}
func (i refItem) FilterValue() string { return i.name }

func newModel() model {
	// Single-line delegate for repos and files
	singleDelegate := list.NewDefaultDelegate()
	singleDelegate.ShowDescription = false
	singleDelegate.SetSpacing(0)

	repos := list.New([]list.Item{}, singleDelegate, 0, 0)
	repos.Title = "Repos"
	repos.SetShowHelp(false)

	files := list.New([]list.Item{}, singleDelegate, 0, 0)
	files.Title = "Files"
	files.SetShowHelp(false)

	// Symbols show both title and description (two lines)
	symbols := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	symbols.Title = "Symbols"
	symbols.SetShowHelp(false)

	deps := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	deps.Title = "Dependencies"
	deps.SetShowHelp(false)

	refs := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	refs.Title = "References"
	refs.SetShowHelp(false)

	vp := viewport.New(0, 0)

	return model{
		repos:    repos,
		files:    files,
		symbols:  symbols,
		deps:     deps,
		refs:     refs,
		viewport: vp,
		mode:     modeThreePane,
		active:   0,
		rstPath:  expandHome(rstDefaultPath),
	}
}

func (m model) Init() tea.Cmd {
	return loadRepos(m.rstPath)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.mode == modeSymbol {
		return m.updateSymbolMode(msg)
	}
	return m.updateThreePaneMode(msg)
}

func (m model) updateThreePaneMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		h := msg.Height - 4
		colWidth := msg.Width / 10
		m.repos.SetSize(colWidth*2, h)  // 20%
		m.files.SetSize(colWidth*4, h)  // 40%
		m.symbols.SetSize(colWidth*4, h) // 40%
		return m, nil

	case reposLoadedMsg:
		m.repos.SetItems(msg.items)
		if len(msg.items) > 0 {
			repo := msg.items[0].(repoItem)
			cmd = loadFiles(m.rstPath, repo.name)
		}
		return m, cmd

	case filesLoadedMsg:
		m.files.SetItems(msg.items)
		if len(msg.items) > 0 {
			repo := m.repos.SelectedItem().(repoItem)
			file := msg.items[0].(fileItem)
			cmd = loadSymbols(m.rstPath, repo.name, file.name)
		}
		return m, cmd

	case symbolsLoadedMsg:
		m.symbols.SetItems(msg.items)
		return m, nil

	case symbolDetailMsg:
		m.symbol = &msg.detail
		m.mode = modeSymbol
		m.viewport.SetContent(msg.detail.code)
		m.deps.SetItems(makeDepsItems(msg.detail.deps))
		m.refs.SetItems(makeRefsItems(msg.detail.refs))
		m.active = 0 // 0: code view
		return m, nil

	case errMsg:
		fmt.Fprintf(os.Stderr, "Error: %v\n", msg.err)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "j":
			switch m.active {
			case 0:
				m.repos.CursorDown()
			case 1:
				m.files.CursorDown()
			case 2:
				m.symbols.CursorDown()
			}
			// Sync selection and load data
			if len(m.repos.Items()) > 0 && m.active == 0 {
				repo := m.repos.SelectedItem().(repoItem)
				cmd = loadFiles(m.rstPath, repo.name)
			}
			if len(m.files.Items()) > 0 && m.active == 1 {
				repo := m.repos.SelectedItem().(repoItem)
				file := m.files.SelectedItem().(fileItem)
				cmd = loadSymbols(m.rstPath, repo.name, file.name)
			}
			return m, cmd
		case "k":
			switch m.active {
			case 0:
				m.repos.CursorUp()
			case 1:
				m.files.CursorUp()
			case 2:
				m.symbols.CursorUp()
			}
			// Sync selection and load data
			if len(m.repos.Items()) > 0 && m.active == 0 {
				repo := m.repos.SelectedItem().(repoItem)
				cmd = loadFiles(m.rstPath, repo.name)
			}
			if len(m.files.Items()) > 0 && m.active == 1 {
				repo := m.repos.SelectedItem().(repoItem)
				file := m.files.SelectedItem().(fileItem)
				cmd = loadSymbols(m.rstPath, repo.name, file.name)
			}
			return m, cmd
		case "h":
			// Move focus left (no wrap)
			if m.active > 0 {
				m.active--
			}
		case "l":
			if m.active < 2 {
				m.active++
			} else if len(m.symbols.Items()) > 0 {
				// Enter symbol detail
				sym := m.symbols.SelectedItem().(symbolItem)
				file := m.files.SelectedItem().(fileItem)
				repo := m.repos.SelectedItem().(repoItem)
				cmd = loadSymbolDetail(m.rstPath, repo.name, file.name, sym.name, sym.line)
			}
		case "enter":
			if m.active == 2 && len(m.symbols.Items()) > 0 {
				sym := m.symbols.SelectedItem().(symbolItem)
				file := m.files.SelectedItem().(fileItem)
				repo := m.repos.SelectedItem().(repoItem)
				cmd = loadSymbolDetail(m.rstPath, repo.name, file.name, sym.name, sym.line)
			}
		}

		// Sync selection when switching focus
		if len(m.repos.Items()) > 0 && m.active == 0 {
			repo := m.repos.SelectedItem().(repoItem)
			cmd = loadFiles(m.rstPath, repo.name)
		}
		if len(m.files.Items()) > 0 && m.active == 1 {
			repo := m.repos.SelectedItem().(repoItem)
			file := m.files.SelectedItem().(fileItem)
			cmd = loadSymbols(m.rstPath, repo.name, file.name)
		}
	}

	return m, cmd
}

func (m model) updateSymbolMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 8
		m.deps.SetSize(msg.Width, 6)
		m.refs.SetSize(msg.Width, 6)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			m.mode = modeThreePane
			m.symbol = nil
			m.symbolStack = nil
			m.active = 2 // Return focus to symbols column
			return m, nil
		case "h":
			// Pop from stack if available, else return to three-pane
			if len(m.symbolStack) > 0 {
				prev := m.symbolStack[len(m.symbolStack)-1]
				m.symbolStack = m.symbolStack[:len(m.symbolStack)-1]
				m.symbol = &symbolDetail{
					name:      prev.name,
					signature: prev.signature,
					filePath:  prev.filePath,
					line:      prev.line,
					deps:      prev.deps,
					refs:      prev.refs,
					code:      prev.code,
				}
				m.viewport.SetContent(prev.code)
				m.deps.SetItems(makeDepsItems(prev.deps))
				m.refs.SetItems(makeRefsItems(prev.refs))
			} else {
				m.mode = modeThreePane
				m.symbol = nil
				m.active = 2 // Return focus to symbols column
			}
			return m, nil
		case "r":
			m.active = 1 // 1: deps list
		case "R":
			m.active = 2 // 2: refs list
		case "j":
			if m.active == 0 {
				m.viewport.LineDown(1)
			} else {
				m.scrollList(1)
			}
		case "k":
			if m.active == 0 {
				m.viewport.LineUp(1)
			} else {
				m.scrollList(-1)
			}
		case "gg":
			m.viewport.GotoTop()
		case "G":
			m.viewport.GotoBottom()
		case "l", "enter":
			// Jump to selected dep/ref
			switch m.active {
			case 1:
				if len(m.deps.Items()) > 0 {
					item := m.deps.SelectedItem().(refItem)
					cmd = m.jumpToSymbol(item.name)
				}
			case 2:
				if len(m.refs.Items()) > 0 {
					item := m.refs.SelectedItem().(refItem)
					cmd = m.jumpToSymbol(item.name)
				}
			}
		}
	}

	return m, cmd
}

func (m *model) scrollList(delta int) {
	var list *list.Model
	if m.active == 1 {
		list = &m.deps
	} else if m.active == 2 {
		list = &m.refs
	} else {
		return
	}

	if len(list.Items()) == 0 {
		return
	}

	currentIdx := list.Index()
	newIdx := currentIdx + delta
	if newIdx < 0 {
		newIdx = 0
	} else if newIdx >= len(list.Items()) {
		newIdx = len(list.Items()) - 1
	}

	// Scroll to keep selection in middle when reaching edges
	visibleHeight := list.Height()
	middlePos := visibleHeight / 2

	if newIdx <= middlePos {
		// Near top, scroll to show from start
		list.SetSize(list.Width(), visibleHeight)
	} else if newIdx >= len(list.Items())-middlePos {
		// Near bottom, scroll to show end
		list.SetSize(list.Width(), visibleHeight)
	}

	list.Select(newIdx)
}

func (m *model) jumpToSymbol(name string) tea.Cmd {
	// Push current symbol to stack
	if m.symbol != nil {
		m.symbolStack = append(m.symbolStack, symbolJump{
			name:      m.symbol.name,
			signature: m.symbol.signature,
			line:      m.symbol.line,
			filePath:  m.symbol.filePath,
			deps:      m.symbol.deps,
			refs:      m.symbol.refs,
			code:      m.symbol.code,
		})
	}
	// Load new symbol detail
	return loadSymbolDetail(m.rstPath, m.repos.SelectedItem().(repoItem).name, "", name, 0)
}

func (m model) View() string {
	if m.mode == modeSymbol {
		return m.viewSymbolPage()
	}
	return m.viewThreePane()
}

func (m model) viewThreePane() string {
	repos := columnStyle.Render(m.repos.View())
	files := columnStyle.Render(m.files.View())

	// Only render symbols when focused on middle or right pane
	var symbols string
	if m.active >= 1 && len(m.symbols.Items()) > 0 {
		symbols = columnStyle.Render(m.symbols.View())
	}

	switch m.active {
	case 0:
		repos = activeColumnStyle.Render(m.repos.View())
	case 1:
		files = activeColumnStyle.Render(m.files.View())
	case 2:
		symbols = activeColumnStyle.Render(m.symbols.View())
	}

	help := helpStyle.Render("h/l: focus | j/k: move | enter/l: select | q: quit | h: back")

	// Two or three pane layout
	if m.active >= 1 {
		return lipgloss.JoinVertical(
			lipgloss.Left,
			lipgloss.JoinHorizontal(lipgloss.Top, repos, files, symbols),
			help,
		)
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, repos, files),
		help,
	)
}

func (m model) viewSymbolPage() string {
	header := fmt.Sprintf("Symbol: %s (%s) | Press q/h to back, r: deps, R: refs, j/k: move, l/enter: jump",
		m.symbol.name, m.symbol.signature)

	codeView := columnStyle.Render(m.viewport.View())

	var lists string
	if m.active == 1 {
		lists = lipgloss.JoinVertical(
			lipgloss.Left,
			depStyle.Render(m.deps.View()),
			refStyle.Render(m.refs.View()),
		)
	} else {
		lists = lipgloss.JoinVertical(
			lipgloss.Left,
			m.deps.View(),
			refStyle.Render(activeColumnStyle.Render(m.refs.View())),
		)
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		titleStyle.Render(header),
		codeView,
		lists,
	)
}

// Commands to load data

func loadRepos(rstPath string) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(rstPath)
		if err != nil {
			return errMsg{err}
		}

		var items []list.Item
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rst") {
				name := rstFileToRepoName(e.Name())
				items = append(items, repoItem{name: name})
			}
		}

		return reposLoadedMsg{items: items}
	}
}

func loadFiles(rstPath, repo string) tea.Cmd {
	return func() tea.Msg {
		rstFile := filepath.Join(rstPath, repoToRSTFile(repo))
		data, err := os.ReadFile(rstFile)
		if err != nil {
			return errMsg{err}
		}

		var r rst.RST
		if err := proto.Unmarshal(data, &r); err != nil {
			return errMsg{err}
		}

		var items []list.Item
		for path := range r.Documents {
			items = append(items, fileItem{name: path})
		}
		// Sort by path for consistent order
		sort.Slice(items, func(i, j int) bool {
			return items[i].(fileItem).name < items[j].(fileItem).name
		})

		return filesLoadedMsg{items: items}
	}
}

func loadSymbols(rstPath, repo, filePath string) tea.Cmd {
	return func() tea.Msg {
		rstFile := filepath.Join(rstPath, repoToRSTFile(repo))
		data, err := os.ReadFile(rstFile)
		if err != nil {
			return errMsg{err}
		}

		var r rst.RST
		if err := proto.Unmarshal(data, &r); err != nil {
			return errMsg{err}
		}

		doc, ok := r.Documents[filePath]
		if !ok {
			return errMsg{fmt.Errorf("file not found: %s", filePath)}
		}

		var items []list.Item
		for symKey, sym := range doc.Symbols {
			items = append(items, symbolItem{
				name:      extractSymbolName(symKey),
				signature: sym.Signature,
				line:      int(sym.Line),
			})
		}
		// Sort by line for consistent order
		sort.Slice(items, func(i, j int) bool {
			return items[i].(symbolItem).line < items[j].(symbolItem).line
		})

		return symbolsLoadedMsg{items: items}
	}
}

type symbolDetail struct {
	name         string
	signature    string
	filePath     string
	line         int
	deps         []string
	refs         []string
	code         string
	symbolKey    string
}

func loadSymbolDetail(rstPath, repo, filePath, symbolName string, line int) tea.Cmd {
	return func() tea.Msg {
		rstFile := filepath.Join(rstPath, repoToRSTFile(repo))
		data, err := os.ReadFile(rstFile)
		if err != nil {
			return errMsg{err}
		}

		var r rst.RST
		if err := proto.Unmarshal(data, &r); err != nil {
			return errMsg{err}
		}

		// Find the document and symbol
		var foundDoc *rst.Document
		var foundSym *rst.Symbol
		var foundPath string

		for path, doc := range r.Documents {
			if filePath != "" && path != filePath {
				continue
			}
			for symKey, sym := range doc.Symbols {
				baseName := extractSymbolName(symKey)
				if baseName == symbolName || baseName == symbolName+"()" {
					if line == 0 || int(sym.Line) == line {
						foundDoc = doc
						foundSym = sym
						foundPath = path
						break
					}
				}
			}
			if foundDoc != nil {
				break
			}
		}

		if foundDoc == nil || foundSym == nil {
			return errMsg{fmt.Errorf("symbol not found: %s", symbolName)}
		}

		return symbolDetailMsg{
			detail: symbolDetail{
				name:      symbolName,
				signature: foundSym.Signature,
				filePath:  foundPath,
				line:      int(foundSym.Line),
				deps:      foundSym.DependenceOn,
				refs:      foundSym.ReferenceBy,
				code:      foundSym.Code,
				symbolKey: extractSymbolKey(foundSym.Symbol),
			},
		}
	}
}

func extractSymbolKey(scipSymbol string) string {
	// Extract the full symbol key from RST
	// Format: `repo/path/file.go`localName` or `repo/path/file.go`Namespace.ClassName`
	lastTick := strings.LastIndex(scipSymbol, "`")
	if lastTick == -1 {
		return scipSymbol
	}
	return scipSymbol[:lastTick+1]
}

func makeDepsItems(deps []string) []list.Item {
	var items []list.Item
	for _, dep := range deps {
		items = append(items, refItem{name: extractSymbolName(dep), kind: "dep"})
	}
	return items
}

func makeRefsItems(refs []string) []list.Item {
	var items []list.Item
	for _, ref := range refs {
		items = append(items, refItem{name: extractSymbolName(ref), kind: "ref"})
	}
	return items
}

// Messages
type reposLoadedMsg struct{ items []list.Item }
type filesLoadedMsg struct{ items []list.Item }
type symbolsLoadedMsg struct{ items []list.Item }
type symbolDetailMsg struct{ detail symbolDetail }
type errMsg struct{ err error }

// Helper functions

func repoToRSTFile(repo string) string {
	name := repo
	name = strings.ReplaceAll(name, ".", "_")
	name = strings.ReplaceAll(name, "/", "_")
	return name + ".go.rst"
}

func rstFileToRepoName(fileName string) string {
	name := strings.TrimSuffix(fileName, ".go.rst")
	name = strings.ReplaceAll(name, "_", ".")
	return name
}

func tuiCommand() cli.Command {
	return cli.Command{
		Name:  "tui",
		Usage: "Interactive TUI for code navigation",
		Description: `Three-pane TUI for navigating code using RST index.
Left pane: Repos (RST files in ~/.rsts)
Middle pane: Files in selected repo
Right pane: Symbols in selected file

Keybindings (three-pane):
  h/l - Move focus left/right (no wrap)
  j/k - Move selection up/down
  enter/l - Select symbol
  h - Move focus left
  q - Quit

Keybindings (symbol detail):
  q - Back to three-pane
  h - Go back to previous symbol
  r - Focus dependencies list
  R - Focus references list
  j/k - Scroll/move
  gg/G - Go to top/bottom
  l/enter - Jump to selected`,
		Action: func(c *cli.Context) error {
			m := newModel()
			p := tea.NewProgram(m, tea.WithAltScreen())
			if err := p.Start(); err != nil {
				return fmt.Errorf("failed to start TUI: %w", err)
			}
			return nil
		},
	}
}
