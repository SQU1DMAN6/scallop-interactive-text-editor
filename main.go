package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
)

type Mode int

const (
	Interactive Mode = iota
	CommandLine
	Find
	PromptSave
	PromptQuit
)

type FileFormat int

const (
	PlainText FileFormat = iota
	Go
	JavaScript
	Python
	HTML
	CSS
	JSON
	Markdown
	Shell
	C
	CPP
	Rust
	Java
	PHP
)

type TokenType int

const (
	TokenNormal TokenType = iota
	TokenKeyword
	TokenString
	TokenComment
	TokenNumber
	TokenOperator
	TokenFunction
	TokenType_
	TokenVariable
	TokenConstant
	TokenClass
	TokenMethod
	TokenProperty
	TokenTag
	TokenAttribute
	TokenValue
	TokenDoctype
	TokenEntity
	TokenSelector
	TokenPseudo
	TokenImportant
	TokenUnit
	TokenPreprocessor
	TokenRegex
	TokenEscape
	TokenDelimiter
	TokenNamespace
	TokenAnnotation
	TokenMacro
)

type Token struct {
	Type    TokenType
	Start   int
	End     int
	Context FileFormat // For embedded syntax
}

type EmbeddedContext struct {
	Format FileFormat
	Start  int
	End    int
}

type SyntaxHighlighter struct {
	format               FileFormat
	keywords             map[string]bool
	patterns             map[TokenType]*regexp.Regexp
	embeddedHighlighters map[FileFormat]*SyntaxHighlighter
}

type AutoClosePair struct {
	open  rune
	close rune
}

type Editor struct {
	lines            []string
	cursorLine       int
	cursorCol        int
	cursorVisualCol  int // visual column accounting for tabs
	horizOffset      int // horizontal scroll offset (in visual columns)
	scrollOffset     int
	fileHandle       *os.File
	fileOffsetLines  int  // how many lines have been loaded from file
	partialLoad      bool // true when file is being streamed (not fully loaded)
	clipboard        string
	mode             Mode
	commandBuf       string
	findBuf          string
	findResults      []int
	findIndex        int
	screen           tcell.Screen
	filename         string
	dirty            bool
	savePending      bool
	quitPending      bool
	format           FileFormat
	highlighter      *SyntaxHighlighter
	autoClosePairs   []AutoClosePair
	lineTokens       [][]Token
	embeddedContexts [][]EmbeddedContext
}

var fileFormat string

func NewEditor() *Editor {
	editor := &Editor{
		lines:  []string{""},
		mode:   Interactive,
		format: PlainText,
		autoClosePairs: []AutoClosePair{
			{'(', ')'},
			{'[', ']'},
			{'{', '}'},
			{'"', '"'},
			{'\'', '\''},
			{'`', '`'},
		},
		lineTokens:       [][]Token{{}},
		embeddedContexts: [][]EmbeddedContext{{}},
		horizOffset:      0,
	}
	editor.highlighter = NewSyntaxHighlighter(PlainText)
	return editor
}

func (e *Editor) Run() {
	s, err := tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	if err := s.Init(); err != nil {
		panic(err)
	}
	e.screen = s
	defer e.screen.Fini()

	// Load file from os.Args (streamed to avoid OOM)
	if len(os.Args) > 1 {
		filename := os.Args[1]
		e.filename = filename
		e.detectFormat()
		f, err := os.Open(filename)
		if err == nil {
			e.fileHandle = f
			e.partialLoad = true
			// read initial chunk
			e.lines = readLinesFromReader(bufio.NewReader(f), 2000)
			e.fileOffsetLines = len(e.lines)
			if len(e.lines) == 0 {
				e.lines = []string{""}
			}
			e.dirty = false
			e.updateSyntaxHighlighting()
		} else {
			// fallback to previous behavior (empty buffer and mark dirty)
			e.filename = filename
			e.dirty = true
			e.detectFormat()
		}
	}

	for {
		e.Render()
		ev := s.PollEvent()
		switch tev := ev.(type) {
		case *tcell.EventKey:
			switch e.mode {
			case Interactive:
				e.handleInteractive(tev)
			case CommandLine:
				e.handleCommandLine(tev)
			case Find:
				e.handleFind(tev)
			case PromptSave:
				e.handlePromptSave(tev)
			case PromptQuit:
				e.handlePromptQuit(tev)
			}
		}
	}
}

// ----------------- INTERACTIVE MODE -----------------

func (e *Editor) handleInteractive(key *tcell.EventKey) {
	ln := e.lines[e.cursorLine]
	ctrl := key.Modifiers()&tcell.ModCtrl != 0

	alt := key.Modifiers()&tcell.ModAlt != 0
	if alt && key.Key() == tcell.KeyLeft {
		e.cursorCol = prevWordStart(ln, e.cursorCol)
		return
	}
	if alt && key.Key() == tcell.KeyRight {
		e.cursorCol = nextWordEnd(ln, e.cursorCol)
		return
	}

	switch key.Key() {
	case tcell.KeyLeft:
		if ctrl {
			e.cursorCol = prevWordStart(ln, e.cursorCol)
		} else if e.cursorCol > 0 {
			e.cursorCol--
			e.updateCursorVisualCol()
		} else if e.cursorLine > 0 {
			e.cursorLine--
			e.cursorCol = len(e.lines[e.cursorLine])
			e.updateCursorVisualCol()
			e.adjustScroll()
		}
	case tcell.KeyRight:
		if ctrl {
			e.cursorCol = nextWordEnd(ln, e.cursorCol)
		} else if e.cursorCol < len(ln) {
			e.cursorCol++
			e.updateCursorVisualCol()
		} else if e.cursorLine < len(e.lines)-1 {
			e.cursorLine++
			e.cursorCol = 0
			e.updateCursorVisualCol()
			e.adjustScroll()
		}
	case tcell.KeyUp:
		if e.cursorLine > 0 {
			e.cursorLine--
			e.fixCursorCol()
			e.adjustScroll()
		}
	case tcell.KeyDown:
		if e.cursorLine < len(e.lines)-1 {
			e.cursorLine++
			e.fixCursorCol()
			e.adjustScroll()
		}
	case tcell.KeyCtrlF:
		e.mode = Find
		e.findBuf = ""
	case tcell.KeyCtrlE:
		e.mode = CommandLine
		e.commandBuf = ""
	case tcell.KeyHome:
		e.cursorCol = 0
	case tcell.KeyEnd:
		e.cursorCol = len(ln)
	case tcell.KeyPgUp:
		e.cursorLine -= (e.pageSize() - 1)
		if e.cursorLine < 0 {
			e.cursorLine = 0
		}
		e.adjustScroll()
	case tcell.KeyPgDn:
		e.cursorLine += (e.pageSize() - 1)
		if e.cursorLine >= len(e.lines) {
			e.cursorLine = len(e.lines) - 1
		}
		e.adjustScroll()
	case tcell.KeyEnter:
		newLine := ""
		if e.cursorCol < len(ln) {
			newLine = ln[e.cursorCol:]
			e.lines[e.cursorLine] = ln[:e.cursorCol]
		}

		// If the character before cursor is an opening bracket and the next char is the matching closing bracket
		if e.cursorCol > 0 && e.cursorCol < len(ln) {
			prev := rune(ln[e.cursorCol-1])
			next := rune(ln[e.cursorCol])
			if (prev == '{' && next == '}') || (prev == '[' && next == ']') || (prev == '(' && next == ')') {
				// insert newline, put closing bracket on its own line and indent
				indent := detectIndentation(e.lines[e.cursorLine])
				innerIndent := indent + "\t"
				// current line becomes up to cursor-1 (including opening bracket)
				left := e.lines[e.cursorLine][:e.cursorCol]
				// ensure left ends with opening bracket
				e.lines[e.cursorLine] = left
				// insert the inner line and the closing bracket line
				// newLine already begins with the closing bracket, so don't prepend it again
				insert := []string{innerIndent, indent + newLine}
				e.lines = append(e.lines[:e.cursorLine+1], append(insert, e.lines[e.cursorLine+1:]...)...)
				e.lineTokens = append(e.lineTokens[:e.cursorLine+1], append([][]Token{{}, {}}, e.lineTokens[e.cursorLine+1:]...)...)
				e.embeddedContexts = append(e.embeddedContexts[:e.cursorLine+1], append([][]EmbeddedContext{{}, {}}, e.embeddedContexts[e.cursorLine+1:]...)...)
				e.cursorLine++
				e.cursorCol = len(innerIndent)
				e.dirty = true
				e.updateSyntaxHighlighting()
				e.adjustScroll()
				return
			}
		}

		e.lines = append(e.lines[:e.cursorLine+1], append([]string{newLine}, e.lines[e.cursorLine+1:]...)...)
		e.lineTokens = append(e.lineTokens[:e.cursorLine+1], append([][]Token{{}}, e.lineTokens[e.cursorLine+1:]...)...)
		e.embeddedContexts = append(e.embeddedContexts[:e.cursorLine+1], append([][]EmbeddedContext{{}}, e.embeddedContexts[e.cursorLine+1:]...)...)
		e.cursorLine++
		e.cursorCol = 0
		e.dirty = true
		e.updateSyntaxHighlighting()
		e.adjustScroll()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if e.cursorCol > 0 {
			// If deleting an opening bracket and next is matching closing bracket and the area is empty, remove both
			if e.cursorCol > 0 && e.cursorCol < len(ln) {
				prev := rune(ln[e.cursorCol-1])
				next := rune(ln[e.cursorCol])
				if (prev == '{' && next == '}') || (prev == '[' && next == ']') || (prev == '(' && next == ')') {
					// If nothing between (cursor was between brackets)
					if e.cursorCol-1 == e.cursorCol-1 {
						// remove closing bracket too
						e.lines[e.cursorLine] = ln[:e.cursorCol-1] + ln[e.cursorCol+1:]
						e.cursorCol--
						e.dirty = true
						e.updateLineTokens(e.cursorLine)
						e.updateCursorVisualCol()
						return
					}
				}
			}

			e.lines[e.cursorLine] = ln[:e.cursorCol-1] + ln[e.cursorCol:]
			e.cursorCol--
			e.dirty = true
			e.updateLineTokens(e.cursorLine)
			e.updateCursorVisualCol()
		} else if e.cursorLine > 0 {
			prev := e.lines[e.cursorLine-1]
			e.lines[e.cursorLine-1] = prev + ln
			e.lines = append(e.lines[:e.cursorLine], e.lines[e.cursorLine+1:]...)
			e.lineTokens = append(e.lineTokens[:e.cursorLine], e.lineTokens[e.cursorLine+1:]...)
			e.embeddedContexts = append(e.embeddedContexts[:e.cursorLine], e.embeddedContexts[e.cursorLine+1:]...)
			e.cursorLine--
			e.cursorCol = len(prev)
			e.dirty = true
			e.updateLineTokens(e.cursorLine)
			e.adjustScroll()
		}
	case tcell.KeyDelete:
		if e.cursorCol < len(ln) {
			e.lines[e.cursorLine] = ln[:e.cursorCol] + ln[e.cursorCol+1:]
			e.dirty = true
			e.updateLineTokens(e.cursorLine)
		} else if e.cursorLine < len(e.lines)-1 {
			e.lines[e.cursorLine] += e.lines[e.cursorLine+1]
			e.lines = append(e.lines[:e.cursorLine+1], e.lines[e.cursorLine+2:]...)
			e.lineTokens = append(e.lineTokens[:e.cursorLine+1], e.lineTokens[e.cursorLine+2:]...)
			e.embeddedContexts = append(e.embeddedContexts[:e.cursorLine+1], e.embeddedContexts[e.cursorLine+2:]...)
			e.dirty = true
			e.updateLineTokens(e.cursorLine)
		}
	case tcell.KeyRune:
		r := key.Rune()
		e.handleRuneInput(r)
		e.dirty = true
	case tcell.KeyTab:
		// Insert a tab character
		ln := e.lines[e.cursorLine]
		e.lines[e.cursorLine] = ln[:e.cursorCol] + "\t" + ln[e.cursorCol:]
		e.cursorCol++
		e.updateLineTokens(e.cursorLine)
		e.updateCursorVisualCol()
		e.dirty = true
	}
}

// detectIndentation returns the leading whitespace (tabs/spaces) of a line
func detectIndentation(line string) string {
	i := 0
	for i < len(line) {
		if line[i] == ' ' || line[i] == '\t' {
			i++
		} else {
			break
		}
	}
	return line[:i]
}

func (e *Editor) fixCursorCol() {
	if e.cursorCol > len(e.lines[e.cursorLine]) {
		e.cursorCol = len(e.lines[e.cursorLine])
	}
}

// ----------------- COMMAND LINE MODE -----------------

func (e *Editor) handleCommandLine(key *tcell.EventKey) {
	switch key.Key() {
	case tcell.KeyEsc:
		e.mode = Interactive
	case tcell.KeyEnter:
		e.executeCommand()
		e.commandBuf = ""
		if !e.savePending && !e.quitPending {
			e.mode = Interactive
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(e.commandBuf) > 0 {
			e.commandBuf = e.commandBuf[:len(e.commandBuf)-1]
		}
	case tcell.KeyRune:
		e.commandBuf += string(key.Rune())
	}
}

func (e *Editor) handlePromptSave(key *tcell.EventKey) {
	switch key.Key() {
	case tcell.KeyEnter:
		filename := strings.TrimSpace(e.commandBuf)
		if filename != "" {
			ioutil.WriteFile(filename, []byte(strings.Join(e.lines, "\n")), 0644)
			e.filename = filename
			e.dirty = false
			e.detectFormat()
			e.updateSyntaxHighlighting()
		}
		e.commandBuf = ""
		e.mode = Interactive
		e.savePending = false
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(e.commandBuf) > 0 {
			e.commandBuf = e.commandBuf[:len(e.commandBuf)-1]
		}
	case tcell.KeyRune:
		e.commandBuf += string(key.Rune())
	}
}

func (e *Editor) handlePromptQuit(key *tcell.EventKey) {
	switch key.Rune() {
	case 'y', 'Y':
		if e.filename != "" {
			ioutil.WriteFile(e.filename, []byte(strings.Join(e.lines, "\n")), 0644)
			e.dirty = false
			e.screen.Fini()
			os.Exit(0)
		} else {
			e.promptSaveCommandLine()
		}
	case 'n', 'N':
		e.screen.Fini()
		os.Exit(0)
	default:
		if e.filename != "" {
			ioutil.WriteFile(e.filename, []byte(strings.Join(e.lines, "\n")), 0644)
			e.dirty = false
			e.screen.Fini()
			os.Exit(0)
		} else {
			e.promptSaveCommandLine()
		}
	}
}

// ----------------- FORMATTING & TESTS -----------------

// formatBuffer formats buffer for supported languages (Go and JSON)
func (e *Editor) formatBuffer() {
	switch e.format {
	case Go:
		src := strings.Join(e.lines, "\n")
		out, err := formatSourceGo(src)
		if err == nil {
			e.lines = strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
			e.dirty = false
			e.updateSyntaxHighlighting()
		} else {
			// keep buffer; optionally show error in status later
		}
	case JSON:
		src := strings.Join(e.lines, "\n")
		out, err := formatJSON(src)
		if err == nil {
			e.lines = strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
			e.dirty = false
			e.updateSyntaxHighlighting()
		}
	default:
		//
	}
}

// runGoTests runs `go test ./...` and returns raw output (stdout+stderr)
func (e *Editor) runGoTests() (string, error) {
	// Use os/exec to run go test
	cmd := exec.Command("go", "test", "./...")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseGoTestOutput extracts file:line:msg style diagnostics
func parseGoTestOutput(output string) []Diagnostic {
	var diags []Diagnostic
	// simple regex for file:line:col: message OR file:line: message
	re := regexp.MustCompile(`([\w\./_-]+\.go):(\d+)(?::(\d+))?:\s*(.*)`)
	matches := re.FindAllStringSubmatch(output, -1)
	for _, m := range matches {
		lineNum := 0
		if n, err := strconv.Atoi(m[2]); err == nil {
			lineNum = n - 1
		}
		diags = append(diags, Diagnostic{File: m[1], Line: lineNum, Message: m[4]})
	}
	return diags
}

type Diagnostic struct {
	File    string
	Line    int
	Message string
}

var diagnostics []Diagnostic

// ----------------- UTIL: run external command -----------------
// (runGoTests uses os/exec directly)

// ----------------- FIND MODE -----------------

func (e *Editor) handleFind(key *tcell.EventKey) {
	switch key.Key() {
	case tcell.KeyEsc:
		e.mode = Interactive
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(e.findBuf) > 0 {
			e.findBuf = e.findBuf[:len(e.findBuf)-1]
			e.updateFindResults()
		}
	case tcell.KeyRune:
		e.findBuf += string(key.Rune())
		e.updateFindResults()
	case tcell.KeyDown, tcell.KeyRight:
		if len(e.findResults) > 0 {
			e.findIndex = (e.findIndex + 1) % len(e.findResults)
			e.gotoFindResult()
		}
	case tcell.KeyUp, tcell.KeyLeft:
		if len(e.findResults) > 0 {
			e.findIndex = (e.findIndex - 1 + len(e.findResults)) % len(e.findResults)
			e.gotoFindResult()
		}
	}
}

// Build the find results only when the search term changes
func (e *Editor) updateFindResults() {
	e.findResults = nil
	if e.findBuf != "" {
		for i, line := range e.lines {
			if strings.Contains(line, e.findBuf) {
				e.findResults = append(e.findResults, i)
			}
		}
		if len(e.findResults) > 0 {
			e.findIndex = 0
			e.gotoFindResult()
		}
	}
}

func (e *Editor) gotoFindResult() {
	if len(e.findResults) == 0 {
		return
	}
	e.cursorLine = e.findResults[e.findIndex]
	e.cursorCol = strings.Index(e.lines[e.cursorLine], e.findBuf)
	e.adjustScroll()
}

// ----------------- EXECUTE COMMAND -----------------

func (e *Editor) executeCommand() {
	args := strings.Fields(e.commandBuf)
	if len(args) == 0 {
		return
	}
	switch strings.ToLower(args[0]) {
	case "exit", "quit":
		if e.dirty {
			e.promptQuitCommandLine()
		} else {
			e.screen.Fini()
			os.Exit(0)
		}
	case "save":
		if len(args) > 1 {
			ioutil.WriteFile(args[1], []byte(strings.Join(e.lines, "\n")), 0644)
			e.filename = args[1]
			e.dirty = false
			e.detectFormat()
			e.updateSyntaxHighlighting()
		} else if e.filename != "" {
			ioutil.WriteFile(e.filename, []byte(strings.Join(e.lines, "\n")), 0644)
			e.dirty = false
		} else {
			e.promptSaveCommandLine()
		}
	case "find":
		e.mode = Find
		e.findBuf = ""
	case "goto":
		if len(args) >= 2 {
			line, err := strconv.Atoi(args[1])
			if err == nil && line >= 1 && line <= len(e.lines) {
				e.cursorLine = line - 1
			}
			if len(args) >= 3 {
				col, err := strconv.Atoi(args[2])
				if err == nil && col >= 0 && col <= len(e.lines[e.cursorLine]) {
					e.cursorCol = col
				}
			} else {
				e.cursorCol = 0
			}
			e.adjustScroll()
		}
	case "format":
		e.formatBuffer()
	case "test":
		out, _ := e.runGoTests()
		diagnostics = parseGoTestOutput(out)
		// if diagnostics pertain to current file, position cursor to first
		if len(diagnostics) > 0 && e.filename != "" {
			for _, d := range diagnostics {
				if filepath.Base(d.File) == filepath.Base(e.filename) {
					if d.Line >= 0 && d.Line < len(e.lines) {
						e.cursorLine = d.Line
						e.adjustScroll()
						break
					}
				}
			}
		}
	}
}

func formatSourceGo(src string) (string, error) {
	out, err := format.Source([]byte(src))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func formatJSON(src string) (string, error) {
	var v interface{}
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (e *Editor) promptSaveCommandLine() {
	e.mode = PromptSave
	e.commandBuf = ""
	e.savePending = true
}

func (e *Editor) promptQuitCommandLine() {
	e.mode = PromptQuit
	e.quitPending = true
}

// ----------------- RENDER -----------------

func (e *Editor) adjustScroll() {
	_, h := e.screen.Size()
	height := h - 5
	if e.cursorLine < e.scrollOffset {
		e.scrollOffset = e.cursorLine
	}
	if e.cursorLine >= e.scrollOffset+height {
		e.scrollOffset = e.cursorLine - height + 1
	}
}

func (e *Editor) pageSize() int {
	_, h := e.screen.Size()
	return h - 5
}

// func expandTabs(s string, tabWidth int) string {
// 	var out strings.Builder
// 	col := 0
// 	for _, r := range s {
// 		if r == '\t' {
// 			spaces := tabWidth - (col % tabWidth)
// 			out.WriteString(strings.Repeat(" ", spaces))
// 			col += spaces
// 		} else {
// 			out.WriteRune(r)
// 			col++
// 		}
// 	}
// 	return out.String()
// }

func (e *Editor) Render() {
	e.screen.Clear()
	w, h := e.screen.Size()
	height := h - 5

	// Top bar
	drawLine(e.screen, 0, 0, w, '-')
	header := "SITE v1, written by SQU1DMAN"
	if e.filename != "" {
		header += " | " + e.filename
		if e.dirty {
			header += " (Modified)"
		}
	}
	header += " | Format: " + string(fileFormat)
	drawString(e.screen, 0, 1, header)
	drawLine(e.screen, 0, 2, w, '-')

	// Interactive space
	lineNumWidth := e.getLineNumberWidth()
	var currentLineNumStr string
	for i := 0; i < height; i++ {
		idx := e.scrollOffset + i
		if idx >= len(e.lines) {
			break
		}
		prefix := " "
		if idx == e.cursorLine {
			prefix = ">"
		}
		lineNumStr := fmt.Sprintf("%*d%s ", lineNumWidth-2, idx+1, prefix)
		if idx == e.cursorLine {
			currentLineNumStr = lineNumStr
		}
		drawString(e.screen, 0, 3+i, lineNumStr)
		// draw with horizontal clipping using expanded tabs
		e.drawHighlightedLineWithHScroll(len(lineNumStr), 3+i, idx, w-lineNumWidth-2)
	}

	// Scroll bar
	if len(e.lines) > 0 {
		topY := 3
		bottomY := 3 + height - 1
		drawString(e.screen, w-1, topY, "▲")
		drawString(e.screen, w-1, bottomY, "▼")
		ratio := float64(e.cursorLine) / float64(max(1, len(e.lines)-1))
		pos := int(ratio * float64(height-1))
		drawString(e.screen, w-1, topY+pos, "█")
	}

	// Diagnostic/status area and command line separator placement
	statusMsg := ""
	if len(diagnostics) > 0 {
		for _, d := range diagnostics {
			if filepath.Base(d.File) == filepath.Base(e.filename) {
				if d.Line == e.cursorLine {
					statusMsg = "Error: " + d.Message
					break
				}
			}
		}
		if statusMsg == "" {
			for _, d := range diagnostics {
				if filepath.Base(d.File) == filepath.Base(e.filename) {
					statusMsg = "Warning: " + d.Message
					break
				}
			}
		}
	}

	statusY := h - 3
	cmdY := h - 1
	if statusMsg != "" {
		drawString(e.screen, 0, statusY+1, statusMsg)
	}
	drawLine(e.screen, 0, cmdY-1, w, '-')

	// Command line
	switch e.mode {
	case CommandLine, PromptSave:
		prompt := "=> "
		if e.savePending {
			prompt += "File name: "
		}
		drawString(e.screen, 0, cmdY, prompt+e.commandBuf)
	case PromptQuit:
		drawString(e.screen, 0, cmdY, "=> Save file? [Y/n] ")
	case Find:
		drawString(e.screen, 0, cmdY, "> "+e.findBuf)
	}

	// Calculate cursor position using actual line number string length and visual columns
	if currentLineNumStr == "" {
		// Fallback if current line is not visible
		prefix := " "
		if e.cursorLine < len(e.lines) {
			prefix = ">"
		}
		currentLineNumStr = fmt.Sprintf("%*d%s ", lineNumWidth-2, e.cursorLine+1, prefix)
	}
	// Ensure cursor visual column is updated
	e.updateCursorVisualCol()
	// Ensure horizontal scroll keeps cursor visible
	if e.cursorVisualCol < e.horizOffset {
		e.horizOffset = e.cursorVisualCol
	}
	maxVisible := w - len(currentLineNumStr) - 2
	if e.cursorVisualCol >= e.horizOffset+maxVisible {
		e.horizOffset = e.cursorVisualCol - maxVisible + 1
	}
	// Place cursor taking horizOffset into account
	screenX := e.cursorVisualCol - e.horizOffset + len(currentLineNumStr)
	e.screen.ShowCursor(screenX, e.cursorLine-e.scrollOffset+3)
	e.screen.Show()
}

// expandTabs returns the visual representation of s with tabs expanded to 4 spaces
func expandTabs(s string) string {
	var out strings.Builder
	col := 0
	tabWidth := 4
	for _, r := range s {
		if r == '\t' {
			spaces := tabWidth - (col % tabWidth)
			out.WriteString(strings.Repeat(" ", spaces))
			col += spaces
		} else {
			out.WriteRune(r)
			col++
		}
	}
	return out.String()
}

// visualColForByteCol computes visual column (expanding tabs) for a byte index within the line
func visualColForByteCol(line string, byteCol int) int {
	col := 0
	for i := 0; i < byteCol && i < len(line); i++ {
		if line[i] == '\t' {
			spaces := 4 - (col % 4)
			col += spaces
		} else {
			col++
		}
	}
	return col
}

// byteColForVisualCol returns approximate byte index for a target visual column
func byteColForVisualCol(line string, target int) int {
	col := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\t' {
			spaces := 4 - (col % 4)
			if col+spaces > target {
				return i
			}
			col += spaces
		} else {
			if col+1 > target {
				return i
			}
			col++
		}
	}
	return len(line)
}

func (e *Editor) updateCursorVisualCol() {
	if e.cursorLine < 0 || e.cursorLine >= len(e.lines) {
		e.cursorVisualCol = 0
		return
	}
	e.cursorVisualCol = visualColForByteCol(e.lines[e.cursorLine], e.cursorCol)
}

// drawHighlightedLineWithHScroll draws tokens but with horizontal clipping
func (e *Editor) drawHighlightedLineWithHScroll(x, y, lineIdx, maxWidth int) {
	if lineIdx >= len(e.lines) {
		return
	}
	line := e.lines[lineIdx]
	var tokens []Token
	if lineIdx < len(e.lineTokens) {
		tokens = e.lineTokens[lineIdx]
	}

	expanded := expandTabs(line)
	totalVis := len(expanded)
	visStart := e.horizOffset
	if visStart < 0 {
		visStart = 0
	}
	visEnd := visStart + maxWidth
	if visEnd > totalVis {
		visEnd = totalVis
	}

	// Precompute visual column for each byte index
	n := len(line)
	visMap := make([]int, n+1)
	for i := 0; i <= n; i++ {
		visMap[i] = visualColForByteCol(line, i)
	}

	// Walk tokens and draw gaps and tokens once
	prevByte := 0
	for _, token := range tokens {
		if token.Start > prevByte {
			gapVisStart := visMap[prevByte]
			gapVisEnd := visMap[token.Start]
			if gapVisEnd > visStart && gapVisStart < visEnd {
				drawFrom := max(gapVisStart, visStart)
				drawTo := min(gapVisEnd, visEnd)
				if drawFrom < drawTo {
					seg := expanded[drawFrom:drawTo]
					drawString(e.screen, x+(drawFrom-visStart), y, seg)
				}
			}
		}

		// token
		tVisStart := visMap[token.Start]
		tVisEnd := visMap[token.End]
		if tVisEnd > visStart && tVisStart < visEnd {
			drawFrom := max(tVisStart, visStart)
			drawTo := min(tVisEnd, visEnd)
			if drawFrom < drawTo {
				seg := expanded[drawFrom:drawTo]
				style := e.getTokenStyle(token.Type)
				startX := x + (drawFrom - visStart)
				for i, r := range seg {
					e.screen.SetContent(startX+i, y, r, nil, style)
				}
			}
		}

		prevByte = token.End
	}

	// trailing gap after last token
	if prevByte < len(line) {
		gapVisStart := visMap[prevByte]
		gapVisEnd := visualColForByteCol(line, len(line))
		if gapVisEnd > visStart && gapVisStart < visEnd {
			drawFrom := max(gapVisStart, visStart)
			drawTo := min(gapVisEnd, visEnd)
			if drawFrom < drawTo {
				seg := expanded[drawFrom:drawTo]
				drawString(e.screen, x+(drawFrom-visStart), y, seg)
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ----------------- HELPERS -----------------

func drawLine(s tcell.Screen, x, y, width int, ch rune) {
	for i := 0; i < width; i++ {
		s.SetContent(x+i, y, ch, nil, tcell.StyleDefault)
	}
}

func drawString(s tcell.Screen, x, y int, str string) {
	for i, r := range str {
		s.SetContent(x+i, y, r, nil, tcell.StyleDefault)
	}
}

func prevWordStart(line string, col int) int {
	if col == 0 {
		return 0
	}
	i := col - 1

	// Skip whitespace to the left
	for i >= 0 && (line[i] == ' ' || line[i] == '\t') {
		i--
	}

	// Skip non-whitespace to find start of current word
	for i >= 0 && line[i] != ' ' && line[i] != '\t' {
		i--
	}

	// Move to start of word
	i++
	if i < 0 {
		i = 0
	}
	return i
}

func nextWordEnd(line string, col int) int {
	if col >= len(line) {
		return len(line)
	}
	i := col

	// Skip whitespace to the right
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}

	// Skip non-whitespace to find end of current word
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}

	return i
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (e *Editor) getLineNumberWidth() int {
	totalLines := len(e.lines)
	if totalLines == 0 {
		return 4 // minimum width: "1> "
	}

	// Calculate digits needed for the largest line number
	digits := 1
	temp := totalLines
	for temp >= 10 {
		digits++
		temp /= 10
	}

	// Add 2 for prefix ("> " or " ") and 1 for space after
	return digits + 3
}

// ----------------- FORMAT DETECTION -----------------

func (e *Editor) detectFormat() {
	if e.filename == "" {
		e.format = PlainText
		e.highlighter = NewSyntaxHighlighter(PlainText)
		return
	}

	ext := strings.ToLower(filepath.Ext(e.filename))
	switch ext {
	case ".go":
		e.format = Go
		fileFormat = "Go"
	case ".js", ".jsx":
		e.format = JavaScript
		fileFormat = "JavaScript"
	case ".py":
		e.format = Python
		fileFormat = "Python"
	case ".html", ".htm":
		e.format = HTML
		fileFormat = "HTML"
	case ".css":
		e.format = CSS
		fileFormat = "CSS"
	case ".json":
		e.format = JSON
		fileFormat = "JSON"
	case ".md", ".markdown":
		e.format = Markdown
		fileFormat = "Markdown"
	case ".sh", ".bash":
		e.format = Shell
		fileFormat = "Shell"
	case ".c":
		e.format = C
		fileFormat = "C"
	case ".cpp", ".cc", ".cxx":
		e.format = CPP
		fileFormat = "CPP"
	case ".rs":
		e.format = Rust
		fileFormat = "Rust"
	case ".java":
		e.format = Java
		fileFormat = "Java"
	case ".php":
		e.format = PHP
		fileFormat = "PHP"
	default:
		e.format = PlainText
		fileFormat = "Plain Text"
	}

	e.highlighter = NewSyntaxHighlighter(e.format)
}

// ----------------- SYNTAX HIGHLIGHTING -----------------

func NewSyntaxHighlighter(format FileFormat) *SyntaxHighlighter {
	h := &SyntaxHighlighter{
		format:               format,
		keywords:             make(map[string]bool),
		patterns:             make(map[TokenType]*regexp.Regexp),
		embeddedHighlighters: make(map[FileFormat]*SyntaxHighlighter),
	}

	switch format {
	case Go:
		h.setupGoHighlighting()
	case JavaScript:
		h.setupJavaScriptHighlighting()
	case Python:
		h.setupPythonHighlighting()
	case HTML:
		h.setupHTMLHighlighting()
	case CSS:
		h.setupCSSHighlighting()
	case Java:
		h.setupJavaHighlighting()
	case Rust:
		h.setupRustHighlighting()
	case C:
		h.setupCHighlighting()
	case CPP:
		h.setupCPPHighlighting()
	case PHP:
		h.setupPHPHighlighting()
	case PlainText:
		// PlainText doesn't need special highlighting, but we still need a valid case
	default:
		h.format = PlainText
	}

	h.setupEmbeddedHighlighters()

	return h
}

func (h *SyntaxHighlighter) setupEmbeddedHighlighters() {
	switch h.format {
	case HTML:
		// HTML can contain JavaScript, CSS, and PHP
		h.embeddedHighlighters[JavaScript] = NewSyntaxHighlighter(JavaScript)
		h.embeddedHighlighters[CSS] = NewSyntaxHighlighter(CSS)
		h.embeddedHighlighters[PHP] = NewSyntaxHighlighter(PHP)
	case PHP:
		// PHP can contain HTML, JavaScript, CSS
		h.embeddedHighlighters[HTML] = NewSyntaxHighlighter(HTML)
		h.embeddedHighlighters[JavaScript] = NewSyntaxHighlighter(JavaScript)
		h.embeddedHighlighters[CSS] = NewSyntaxHighlighter(CSS)
	case Shell:
		// Shell scripts can contain embedded code
		h.embeddedHighlighters[HTML] = NewSyntaxHighlighter(HTML)
		h.embeddedHighlighters[JavaScript] = NewSyntaxHighlighter(JavaScript)
		h.embeddedHighlighters[Python] = NewSyntaxHighlighter(Python)
	}
}

func (h *SyntaxHighlighter) setupGoHighlighting() {
	keywords := []string{
		"break", "case", "chan", "const", "continue", "default", "defer", "else",
		"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
		"map", "package", "range", "return", "select", "struct", "switch", "type",
		"var", "bool", "byte", "complex64", "complex128", "error", "float32",
		"float64", "int", "int8", "int16", "int32", "int64", "rune", "string",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "true", "false",
		"iota", "nil", "append", "cap", "close", "complex", "copy", "delete",
		"imag", "len", "make", "new", "panic", "print", "println", "real", "recover",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|` + "`[^`]*`" + `|'([^'\\]|\\.)*'`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b|0[xX][0-9a-fA-F]+|0[0-7]+`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|:=|<<|>>|\+\+|--|&&|\|\||<-`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenType_] = regexp.MustCompile(`\b[A-Z][a-zA-Z0-9_]*\b`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
	h.patterns[TokenVariable] = regexp.MustCompile(`\b[a-z_][a-zA-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupJavaScriptHighlighting() {
	keywords := []string{
		"break", "case", "catch", "class", "const", "continue", "debugger", "default",
		"delete", "do", "else", "export", "extends", "finally", "for", "function",
		"if", "import", "in", "instanceof", "let", "new", "return", "super", "switch",
		"this", "throw", "try", "typeof", "var", "void", "while", "with", "yield",
		"true", "false", "null", "undefined", "async", "await",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'|` + "`[^`]*`")
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b|0[xX][0-9a-fA-F]+|0[bB][01]+|0[oO][0-7]+`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|===|!==|==|!=|<=|>=|\+\+|--|&&|\|\||=>`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\b[a-zA-Z_$][a-zA-Z0-9_$]*\s*\(|function\s+[a-zA-Z_$][a-zA-Z0-9_$]*`)
	h.patterns[TokenClass] = regexp.MustCompile(`\bclass\s+[a-zA-Z_$][a-zA-Z0-9_$]*|\bnew\s+[A-Z][a-zA-Z0-9_$]*`)
	h.patterns[TokenMethod] = regexp.MustCompile(`\.[a-zA-Z_$][a-zA-Z0-9_$]*\s*\(`)
	h.patterns[TokenProperty] = regexp.MustCompile(`\.[a-zA-Z_$][a-zA-Z0-9_$]*`)
	h.patterns[TokenRegex] = regexp.MustCompile(`/(?:[^/\\\n]|\\.)+/[gimuy]*`)
}

func (h *SyntaxHighlighter) setupPythonHighlighting() {
	keywords := []string{
		"and", "as", "assert", "break", "class", "continue", "def", "del", "elif",
		"else", "except", "exec", "finally", "for", "from", "global", "if", "import",
		"in", "is", "lambda", "not", "or", "pass", "print", "raise", "return", "try",
		"while", "with", "yield", "True", "False", "None", "async", "await", "nonlocal",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"""[\s\S]*?"""|'''[\s\S]*?'''|"([^"\\]|\\.)*"|'([^'\\]|\\.)*'|r"[^"]*"|r'[^']*'`)
	h.patterns[TokenComment] = regexp.MustCompile(`#.*$`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b|0[xX][0-9a-fA-F]+|0[bB][01]+|0[oO][0-7]+`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|==|!=|<=|>=|\*\*|//|@`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\bdef\s+[a-zA-Z_][a-zA-Z0-9_]*|[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenClass] = regexp.MustCompile(`\bclass\s+[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenMethod] = regexp.MustCompile(`\.[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenProperty] = regexp.MustCompile(`\.[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenAnnotation] = regexp.MustCompile(`@[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupHTMLHighlighting() {
	keywords := []string{
		"html", "head", "body", "title", "meta", "script", "style", "link", "div", "span", "p",
		"h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "a", "img", "table", "tr", "td",
		"th", "caption", "form", "input", "textarea", "button", "select", "option", "label",
		"br", "hr", "blockquote", "cite", "code", "pre", "kbd", "samp", "var", "small", "strong",
		"em", "b", "i", "u", "s", "del", "ins", "sup", "sub", "mark", "ruby", "rt", "rp", "bdi",
		"bdo", "iframe", "picture", "source", "video", "audio", "track", "canvas", "map", "area",
		"base", "nav", "section", "article", "aside", "header", "footer", "main", "figure",
		"figcaption", "details", "summary", "dialog", "menu", "menuitem",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenComment] = regexp.MustCompile(`<!--[\s\S]*?-->`)
	h.patterns[TokenDoctype] = regexp.MustCompile(`<!DOCTYPE[^>]*>`)
	h.patterns[TokenTag] = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*`)
	h.patterns[TokenAttribute] = regexp.MustCompile(`\b[a-zA-Z-]+\s*=`)
	h.patterns[TokenValue] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)
	h.patterns[TokenEntity] = regexp.MustCompile(`&[a-zA-Z][a-zA-Z0-9]*;|&#\d+;|&#x[0-9a-fA-F]+;`)
	h.patterns[TokenDelimiter] = regexp.MustCompile(`[<>/=]`)
}

func (h *SyntaxHighlighter) setupCSSHighlighting() {
	keywords := []string{
		"color", "background", "font", "margin", "padding", "border", "width", "height",
		"display", "position", "top", "left", "right", "bottom", "float", "clear",
		"text-align", "text-decoration", "font-size", "font-weight", "font-family",
		"line-height", "letter-spacing", "word-spacing", "white-space", "vertical-align",
		"list-style", "overflow", "visibility", "z-index", "cursor", "opacity",
		"transform", "transition", "animation", "flex", "grid", "justify-content",
		"align-items", "align-content", "flex-direction", "flex-wrap", "order",
		"flex-grow", "flex-shrink", "flex-basis", "grid-template", "grid-area",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenComment] = regexp.MustCompile(`/\*[\s\S]*?\*/`)
	h.patterns[TokenSelector] = regexp.MustCompile(`[.#]?[a-zA-Z][a-zA-Z0-9_-]*\s*[{,]|[a-zA-Z][a-zA-Z0-9_-]*\s*:`)
	h.patterns[TokenProperty] = regexp.MustCompile(`[a-zA-Z-]+\s*:`)
	h.patterns[TokenValue] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?(px|em|rem|%|vh|vw|pt|pc|in|cm|mm|ex|ch|vmin|vmax|fr)?\b`)
	h.patterns[TokenUnit] = regexp.MustCompile(`(px|em|rem|%|vh|vw|pt|pc|in|cm|mm|ex|ch|vmin|vmax|fr)\b`)
	h.patterns[TokenPseudo] = regexp.MustCompile(`:[a-zA-Z-]+(\([^)]*\))?`)
	h.patterns[TokenImportant] = regexp.MustCompile(`!important\b`)
	h.patterns[TokenDelimiter] = regexp.MustCompile(`[{}();:,]`)
}

func (h *SyntaxHighlighter) setupJavaHighlighting() {
	keywords := []string{
		"abstract", "assert", "boolean", "break", "byte", "case", "catch", "char", "class",
		"const", "continue", "default", "do", "double", "else", "enum", "extends", "final",
		"finally", "float", "for", "goto", "if", "implements", "import", "instanceof", "int",
		"interface", "long", "native", "new", "package", "private", "protected", "public",
		"return", "short", "static", "strictfp", "super", "switch", "synchronized", "this",
		"throw", "throws", "transient", "try", "void", "volatile", "while", "true", "false", "null",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?[fFdDlL]?\b|0[xX][0-9a-fA-F]+[lL]?|0[bB][01]+[lL]?|0[0-7]+[lL]?`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|==|!=|<=|>=|\+\+|--|&&|\|\||<<|>>|>>>`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenClass] = regexp.MustCompile(`\b[A-Z][a-zA-Z0-9_]*\b`)
	h.patterns[TokenAnnotation] = regexp.MustCompile(`@[A-Z][a-zA-Z0-9_]*`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupRustHighlighting() {
	keywords := []string{
		"as", "break", "const", "continue", "crate", "else", "enum", "extern", "false", "fn",
		"for", "if", "impl", "in", "let", "loop", "match", "mod", "move", "mut", "pub", "ref",
		"return", "self", "Self", "static", "struct", "super", "trait", "true", "type", "unsafe",
		"use", "where", "while", "async", "await", "dyn", "abstract", "become", "box", "do",
		"final", "macro", "override", "priv", "typeof", "unsized", "virtual", "yield",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'|r#"[^"]*"#|r"[^"]*"`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?[fF]?\b|0[xX][0-9a-fA-F]+|0[bB][01]+|0[oO][0-7]+`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|==|!=|<=|>=|\+\+|--|&&|\|\||<<|>>|->|=>`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\bfn\s+[a-zA-Z_][a-zA-Z0-9_]*|[a-zA-Z_][a-zA-Z0-9_]*\s*!?\s*\(`)
	h.patterns[TokenMacro] = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*!`)
	h.patterns[TokenType_] = regexp.MustCompile(`\b[A-Z][a-zA-Z0-9_]*\b`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupCHighlighting() {
	keywords := []string{
		"auto", "break", "case", "char", "const", "continue", "default", "do", "double", "else",
		"enum", "extern", "float", "for", "goto", "if", "inline", "int", "long", "register",
		"restrict", "return", "short", "signed", "sizeof", "static", "struct", "switch",
		"typedef", "union", "unsigned", "void", "volatile", "while", "_Alignas", "_Alignof",
		"_Atomic", "_Static_assert", "_Noreturn", "_Thread_local", "_Generic",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?[fFlL]?\b|0[xX][0-9a-fA-F]+[uUlL]*|0[0-7]+[uUlL]*`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|==|!=|<=|>=|\+\+|--|&&|\|\||<<|>>|->`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenPreprocessor] = regexp.MustCompile(`#\s*[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenType_] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*_t\b`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupCPPHighlighting() {
	keywords := []string{
		"alignas", "alignof", "and", "and_eq", "asm", "auto", "bitand", "bitor", "bool", "break",
		"case", "catch", "char", "char16_t", "char32_t", "class", "compl", "const", "constexpr",
		"const_cast", "continue", "decltype", "default", "delete", "do", "double", "dynamic_cast",
		"else", "enum", "explicit", "export", "extern", "false", "float", "for", "friend", "goto",
		"if", "inline", "int", "long", "mutable", "namespace", "new", "noexcept", "not", "not_eq",
		"nullptr", "operator", "or", "or_eq", "private", "protected", "public", "register",
		"reinterpret_cast", "return", "short", "signed", "sizeof", "static", "static_assert",
		"static_cast", "struct", "switch", "template", "this", "thread_local", "throw", "true",
		"try", "typedef", "typeid", "typename", "union", "unsigned", "using", "virtual", "void",
		"volatile", "wchar_t", "while", "xor", "xor_eq",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'|R"\([^)]*\)"`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?[fFlL]?\b|0[xX][0-9a-fA-F]+[uUlL]*|0[bB][01]+[uUlL]*|0[0-7]+[uUlL]*`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|==|!=|<=|>=|\+\+|--|&&|\|\||<<|>>|->|\*|::`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenClass] = regexp.MustCompile(`\bclass\s+[a-zA-Z_][a-zA-Z0-9_]*|[A-Z][a-zA-Z0-9_]*`)
	h.patterns[TokenNamespace] = regexp.MustCompile(`\bnamespace\s+[a-zA-Z_][a-zA-Z0-9_]*|[a-zA-Z_][a-zA-Z0-9_]*::`)
	h.patterns[TokenPreprocessor] = regexp.MustCompile(`#\s*[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenType_] = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*_t\b`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
}

func (h *SyntaxHighlighter) setupPHPHighlighting() {
	keywords := []string{
		"abstract", "and", "array", "as", "break", "callable", "case", "catch", "class", "clone",
		"const", "continue", "declare", "default", "die", "do", "echo", "else", "elseif", "empty",
		"enddeclare", "endfor", "endforeach", "endif", "endswitch", "endwhile", "eval", "exit",
		"extends", "final", "finally", "for", "foreach", "function", "global", "goto", "if",
		"implements", "include", "include_once", "instanceof", "insteadof", "interface", "isset",
		"list", "namespace", "new", "or", "print", "private", "protected", "public", "require",
		"require_once", "return", "static", "switch", "throw", "trait", "try", "unset", "use",
		"var", "while", "xor", "yield", "true", "false", "null", "__CLASS__", "__DIR__", "__FILE__",
		"__FUNCTION__", "__LINE__", "__METHOD__", "__NAMESPACE__", "__TRAIT__",
	}

	for _, keyword := range keywords {
		h.keywords[keyword] = true
	}

	h.patterns[TokenString] = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'|<<<[A-Z_][A-Z0-9_]*[\s\S]*?^[A-Z_][A-Z0-9_]*`)
	h.patterns[TokenComment] = regexp.MustCompile(`//.*$|/\*[\s\S]*?\*/|#.*$`)
	h.patterns[TokenNumber] = regexp.MustCompile(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b|0[xX][0-9a-fA-F]+|0[bB][01]+|0[oO][0-7]+`)
	h.patterns[TokenOperator] = regexp.MustCompile(`[+\-*/=<>!&|^%]+|===|!==|==|!=|<=|>=|\*\*|//|\.|\?:|\?\?`)
	h.patterns[TokenVariable] = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenFunction] = regexp.MustCompile(`\bfunction\s+[a-zA-Z_][a-zA-Z0-9_]*|[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenClass] = regexp.MustCompile(`\bclass\s+[a-zA-Z_][a-zA-Z0-9_]*|[A-Z][a-zA-Z0-9_]*`)
	h.patterns[TokenMethod] = regexp.MustCompile(`->[a-zA-Z_][a-zA-Z0-9_]*\s*\(|::[a-zA-Z_][a-zA-Z0-9_]*\s*\(`)
	h.patterns[TokenProperty] = regexp.MustCompile(`->[a-zA-Z_][a-zA-Z0-9_]*|::[a-zA-Z_][a-zA-Z0-9_]*`)
	h.patterns[TokenConstant] = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b|__[A-Z_]+__`)
}

func (e *Editor) updateSyntaxHighlighting() {
	e.lineTokens = make([][]Token, len(e.lines))
	e.embeddedContexts = make([][]EmbeddedContext, len(e.lines))
	for i := range e.lines {
		e.updateLineTokens(i)
	}
}

func (e *Editor) updateLineTokens(lineIdx int) {
	if lineIdx >= len(e.lines) || lineIdx >= len(e.lineTokens) {
		return
	}

	line := e.lines[lineIdx]
	if e.highlighter == nil {
		e.lineTokens[lineIdx] = []Token{}
		e.embeddedContexts[lineIdx] = []EmbeddedContext{}
		return
	}
	e.lineTokens[lineIdx], e.embeddedContexts[lineIdx] = e.highlighter.tokenizeLineWithContext(line)
}

func (h *SyntaxHighlighter) tokenizeLineWithContext(line string) ([]Token, []EmbeddedContext) {
	if h.format == PlainText {
		return []Token{}, []EmbeddedContext{}
	}

	var tokens []Token

	// Detect embedded contexts first
	contexts := h.detectEmbeddedContexts(line)

	// Tokenize the main language
	tokens = h.tokenizeLine(line)

	// Tokenize embedded contexts
	for _, ctx := range contexts {
		if embeddedHighlighter, exists := h.embeddedHighlighters[ctx.Format]; exists {
			embeddedText := line[ctx.Start:ctx.End]
			embeddedTokens := embeddedHighlighter.tokenizeLine(embeddedText)

			// Adjust token positions and add context
			for _, token := range embeddedTokens {
				adjustedToken := Token{
					Type:    token.Type,
					Start:   token.Start + ctx.Start,
					End:     token.End + ctx.Start,
					Context: ctx.Format,
				}
				tokens = append(tokens, adjustedToken)
			}
		}
	}

	// Sort tokens by start position
	for i := 0; i < len(tokens); i++ {
		for j := i + 1; j < len(tokens); j++ {
			if tokens[i].Start > tokens[j].Start {
				tokens[i], tokens[j] = tokens[j], tokens[i]
			}
		}
	}

	return tokens, contexts
}

func (h *SyntaxHighlighter) detectEmbeddedContexts(line string) []EmbeddedContext {
	var contexts []EmbeddedContext

	switch h.format {
	case HTML:
		// Detect <script> tags for JavaScript
		scriptPattern := regexp.MustCompile(`<script[^>]*>(.*?)</script>`)
		matches := scriptPattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) >= 4 {
				contexts = append(contexts, EmbeddedContext{
					Format: JavaScript,
					Start:  match[2],
					End:    match[3],
				})
			}
		}

		// Detect <style> tags for CSS
		stylePattern := regexp.MustCompile(`<style[^>]*>(.*?)</style>`)
		matches = stylePattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) >= 4 {
				contexts = append(contexts, EmbeddedContext{
					Format: CSS,
					Start:  match[2],
					End:    match[3],
				})
			}
		}

		// Detect PHP tags
		phpPattern := regexp.MustCompile(`<\?php(.*?)\?>|<\?(.*?)\?>`)
		matches = phpPattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) >= 4 {
				start := match[2]
				end := match[3]
				if start == -1 && len(match) >= 6 {
					start = match[4]
					end = match[5]
				}
				if start != -1 {
					contexts = append(contexts, EmbeddedContext{
						Format: PHP,
						Start:  start,
						End:    end,
					})
				}
			}
		}

	case PHP:
		// Detect HTML outside PHP tags
		htmlPattern := regexp.MustCompile(`\?>([^<]*(?:<[^<]*)*)<\?php`)
		matches := htmlPattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) >= 4 {
				contexts = append(contexts, EmbeddedContext{
					Format: HTML,
					Start:  match[2],
					End:    match[3],
				})
			}
		}

	case Shell:
		// Detect heredoc with HTML
		heredocPattern := regexp.MustCompile(`<<['"]*HTML['"]*\s*(.*?)\s*HTML`)
		matches := heredocPattern.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) >= 4 {
				contexts = append(contexts, EmbeddedContext{
					Format: HTML,
					Start:  match[2],
					End:    match[3],
				})
			}
		}
	}

	return contexts
}

func (h *SyntaxHighlighter) tokenizeLine(line string) []Token {
	if h.format == PlainText {
		return []Token{}
	}

	var tokens []Token

	// First, find strings and comments (they have priority)
	for tokenType, pattern := range h.patterns {
		if tokenType == TokenString || tokenType == TokenComment {
			matches := pattern.FindAllStringIndex(line, -1)
			for _, match := range matches {
				tokens = append(tokens, Token{
					Type:    tokenType,
					Start:   match[0],
					End:     match[1],
					Context: h.format,
				})
			}
		}
	}

	// Sort tokens by start position
	for i := 0; i < len(tokens); i++ {
		for j := i + 1; j < len(tokens); j++ {
			if tokens[i].Start > tokens[j].Start {
				tokens[i], tokens[j] = tokens[j], tokens[i]
			}
		}
	}

	// Find keywords and other tokens, but skip areas already covered by strings/comments
	words := regexp.MustCompile(`\b\w+\b`).FindAllStringIndex(line, -1)
	for _, wordMatch := range words {
		if h.isPositionCovered(wordMatch[0], wordMatch[1], tokens) {
			continue
		}

		word := line[wordMatch[0]:wordMatch[1]]
		if h.keywords[word] {
			tokens = append(tokens, Token{
				Type:    TokenKeyword,
				Start:   wordMatch[0],
				End:     wordMatch[1],
				Context: h.format,
			})
		}
	}

	// Find numbers and operators
	for tokenType, pattern := range h.patterns {
		if tokenType != TokenString && tokenType != TokenComment {
			matches := pattern.FindAllStringIndex(line, -1)
			for _, match := range matches {
				if !h.isPositionCovered(match[0], match[1], tokens) {
					tokens = append(tokens, Token{
						Type:    tokenType,
						Start:   match[0],
						End:     match[1],
						Context: h.format,
					})
				}
			}
		}
	}

	// Sort tokens by start position again
	for i := 0; i < len(tokens); i++ {
		for j := i + 1; j < len(tokens); j++ {
			if tokens[i].Start > tokens[j].Start {
				tokens[i], tokens[j] = tokens[j], tokens[i]
			}
		}
	}

	return tokens
}

func (h *SyntaxHighlighter) isPositionCovered(start, end int, tokens []Token) bool {
	for _, token := range tokens {
		if start >= token.Start && end <= token.End {
			return true
		}
	}
	return false
}

func (e *Editor) drawHighlightedLine(x, y, lineIdx int) {
	if lineIdx >= len(e.lines) {
		return
	}

	line := e.lines[lineIdx]
	if lineIdx >= len(e.lineTokens) {
		drawString(e.screen, x, y, line)
		return
	}

	tokens := e.lineTokens[lineIdx]
	pos := 0

	for _, token := range tokens {
		// Draw text before token
		if token.Start > pos {
			drawString(e.screen, x+pos, y, line[pos:token.Start])
		}

		// Draw token with highlighting
		style := e.getTokenStyle(token.Type)
		tokenText := line[token.Start:token.End]
		for i, r := range tokenText {
			e.screen.SetContent(x+token.Start+i, y, r, nil, style)
		}

		pos = token.End
	}

	// Draw remaining text
	if pos < len(line) {
		drawString(e.screen, x+pos, y, line[pos:])
	}
}

// readLinesFromReader reads up to maxLines from reader and returns the lines slice.
// It stops early on EOF and returns whatever it read.
func readLinesFromReader(r *bufio.Reader, maxLines int) []string {
	var lines []string
	for i := 0; i < maxLines; i++ {
		ln, err := r.ReadString('\n')
		if err != nil {
			if len(ln) > 0 {
				// strip trailing newline if present
				ln = strings.TrimRight(ln, "\n")
				lines = append(lines, ln)
			}
			break
		}
		// remove trailing newline
		lines = append(lines, strings.TrimRight(ln, "\n"))
	}
	return lines
}

// loadMoreLines appends up to n more lines from the open fileHandle into e.lines.
// If EOF is reached it closes the file and marks partialLoad=false.
func (e *Editor) loadMoreLines(n int) {
	if e.fileHandle == nil || !e.partialLoad {
		return
	}
	r := bufio.NewReader(e.fileHandle)
	newLines := readLinesFromReader(r, n)
	if len(newLines) > 0 {
		e.lines = append(e.lines, newLines...)
		e.fileOffsetLines = len(e.lines)
		e.updateSyntaxHighlighting()
	}
	// Try to peek to see if EOF
	_, err := r.Peek(1)
	if err != nil {
		// assume EOF or unreadable -> close and mark fully loaded
		e.fileHandle.Close()
		e.fileHandle = nil
		e.partialLoad = false
	}
}

func (e *Editor) getTokenStyle(tokenType TokenType) tcell.Style {
	switch tokenType {
	case TokenKeyword:
		return tcell.StyleDefault.Foreground(tcell.ColorTurquoise).Bold(true)
	case TokenString, TokenValue, TokenRegex:
		return tcell.StyleDefault.Foreground(tcell.ColorLightGreen) // green for literals
	case TokenComment, TokenDoctype, TokenPreprocessor:
		return tcell.StyleDefault.Foreground(tcell.NewRGBColor(0, 101, 0)).Italic(true)
	case TokenNumber, TokenConstant, TokenUnit, TokenEscape:
		return tcell.StyleDefault.Foreground(tcell.ColorOrange) // yellow for numbers/consts
	case TokenOperator, TokenImportant, TokenMacro, TokenTag:
		return tcell.StyleDefault.Foreground(tcell.ColorRed) // red for operators/tags
	case TokenFunction, TokenMethod:
		return tcell.StyleDefault.Foreground(tcell.NewRGBColor(54, 54, 235).TrueColor())
	case TokenType_, TokenClass, TokenAttribute:
		return tcell.StyleDefault.Foreground(tcell.ColorLightBlue) // blue for types/classes
	case TokenVariable, TokenDelimiter:
		return tcell.StyleDefault.Foreground(tcell.NewRGBColor(44, 44, 178).TrueColor())
	case TokenProperty, TokenPseudo, TokenAnnotation, TokenNamespace:
		return tcell.StyleDefault.Foreground(tcell.ColorOrchid)
	default:
		return tcell.StyleDefault
	}
}

// ----------------- AUTO-CLOSING BRACKETS/QUOTES -----------------

func (e *Editor) handleRuneInput(r rune) {
	ln := e.lines[e.cursorLine]

	// Check for auto-closing pairs
	for _, pair := range e.autoClosePairs {
		if r == pair.open {
			if pair.open == pair.close {
				// Handle quotes (same open/close character)
				if e.shouldAutoCloseQuote(r, ln) {
					e.insertAutoClosePair(r, pair.close)
					return
				}
			} else {
				// Handle brackets/parentheses
				e.insertAutoClosePair(r, pair.close)
				return
			}
		} else if r == pair.close && pair.open != pair.close {
			// Skip closing bracket if it's already there
			if e.cursorCol < len(ln) && rune(ln[e.cursorCol]) == r {
				e.cursorCol++
				return
			}
		}
	}

	// Regular character insertion
	e.lines[e.cursorLine] = ln[:e.cursorCol] + string(r) + ln[e.cursorCol:]
	e.cursorCol++
	e.updateLineTokens(e.cursorLine)
}

func (e *Editor) shouldAutoCloseQuote(quote rune, line string) bool {
	// Count quotes before cursor position
	count := 0
	for i := 0; i < e.cursorCol && i < len(line); i++ {
		if rune(line[i]) == quote {
			count++
		}
	}
	// Auto-close if we have an even number of quotes (starting a new pair)
	return count%2 == 0
}

func (e *Editor) insertAutoClosePair(open, close rune) {
	ln := e.lines[e.cursorLine]
	e.lines[e.cursorLine] = ln[:e.cursorCol] + string(open) + string(close) + ln[e.cursorCol:]
	e.cursorCol++
	e.updateLineTokens(e.cursorLine)
}

// ----------------- MAIN -----------------

func main() {
	editor := NewEditor()
	editor.Run()
}
