package treesitter

import (
	"context"
	"fmt"
	"sync"

	"github.com/gofrs/uuid"
	"github.com/uber/scip-lsp/src/ulsp/controller/diagnostics"
	ulspplugin "github.com/uber/scip-lsp/src/ulsp/entity/ulsp-plugin"
	"github.com/uber/scip-lsp/src/ulsp/repository/session"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const _nameKey = "tree-sitter"

// Controller is the interface for the tree-sitter plugin.
type Controller interface {
	StartupInfo(ctx context.Context) (ulspplugin.PluginInfo, error)
}

// ParseTree represents a parsed syntax tree for a document.
type ParseTree struct {
	URI     protocol.DocumentURI
	Content string
}

type parseTreeStore map[uuid.UUID]map[protocol.DocumentURI]*ParseTree

// Params are the parameters to set up this controller.
type Params struct {
	fx.In

	Logger      *zap.SugaredLogger
	Sessions    session.Repository
	Diagnostics diagnostics.Controller
}

type controller struct {
	logger      *zap.SugaredLogger
	sessions    session.Repository
	diagnostics diagnostics.Controller
	parseTrees  parseTreeStore
	mu          sync.RWMutex
}

// New creates a new tree-sitter controller.
func New(p Params) Controller {
	return &controller{
		logger:      p.Logger.With("plugin", _nameKey),
		sessions:    p.Sessions,
		diagnostics: p.Diagnostics,
		parseTrees:  make(parseTreeStore),
	}
}

// StartupInfo returns PluginInfo for this controller.
func (c *controller) StartupInfo(ctx context.Context) (ulspplugin.PluginInfo, error) {
	priorities := map[string]ulspplugin.Priority{
		protocol.MethodInitialize:            ulspplugin.PriorityHigh,
		protocol.MethodTextDocumentDidOpen:   ulspplugin.PriorityAsync,
		protocol.MethodTextDocumentDidChange: ulspplugin.PriorityAsync,
		protocol.MethodTextDocumentDidClose:  ulspplugin.PriorityAsync,
		ulspplugin.MethodEndSession:          ulspplugin.PriorityRegular,
	}

	methods := &ulspplugin.Methods{
		PluginNameKey: _nameKey,

		Initialize: c.initialize,
		DidOpen:    c.didOpen,
		DidChange:  c.didChange,
		DidClose:   c.didClose,
		EndSession: c.endSession,
	}

	return ulspplugin.PluginInfo{
		Priorities: priorities,
		Methods:    methods,
		NameKey:    _nameKey,
	}, nil
}

// initialize sets up the parse tree store for a new session.
func (c *controller) initialize(ctx context.Context, params *protocol.InitializeParams, result *protocol.InitializeResult) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.parseTrees[s.UUID] = make(map[protocol.DocumentURI]*ParseTree)
	return nil
}

// didOpen parses a newly opened document and reports any syntax diagnostics.
func (c *controller) didOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	docURI := params.TextDocument.URI
	tree := c.parse(docURI, params.TextDocument.Text)

	c.mu.Lock()
	if c.parseTrees[s.UUID] != nil {
		c.parseTrees[s.UUID][docURI] = tree
	}
	c.mu.Unlock()

	diags := syntaxDiagnostics(tree)
	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), diags); err != nil {
		c.logger.Warnw("failed to publish diagnostics", "uri", docURI, "error", err)
	}
	c.logger.Debugw("parsed document", "uri", docURI, "diagnostics", len(diags))
	return nil
}

// didChange re-parses a document after it has changed and updates diagnostics.
func (c *controller) didChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	if len(params.ContentChanges) == 0 {
		return nil
	}

	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	docURI := params.TextDocument.URI
	text := params.ContentChanges[len(params.ContentChanges)-1].Text
	tree := c.parse(docURI, text)

	c.mu.Lock()
	if c.parseTrees[s.UUID] != nil {
		c.parseTrees[s.UUID][docURI] = tree
	}
	c.mu.Unlock()

	diags := syntaxDiagnostics(tree)
	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), diags); err != nil {
		c.logger.Warnw("failed to publish diagnostics", "uri", docURI, "error", err)
	}
	c.logger.Debugw("re-parsed document", "uri", docURI, "diagnostics", len(diags))
	return nil
}

// didClose removes the parse tree for a closed document and clears its diagnostics.
func (c *controller) didClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	docURI := params.TextDocument.URI

	c.mu.Lock()
	if c.parseTrees[s.UUID] != nil {
		delete(c.parseTrees[s.UUID], docURI)
	}
	c.mu.Unlock()

	// Clear diagnostics for this document when it is closed.
	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), nil); err != nil {
		c.logger.Warnw("failed to clear diagnostics", "uri", docURI, "error", err)
	}
	return nil
}

// endSession cleans up all parse trees for a closed session.
func (c *controller) endSession(ctx context.Context, id uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.parseTrees, id)
	return nil
}

// parse creates a ParseTree for the given document URI and content.
func (c *controller) parse(docURI protocol.DocumentURI, content string) *ParseTree {
	return &ParseTree{URI: docURI, Content: content}
}

// syntaxDiagnostics returns diagnostics for basic bracket-matching errors.
// This acts as a placeholder for real tree-sitter parsing, which can be
// integrated here once tree-sitter Go bindings are available.
func syntaxDiagnostics(tree *ParseTree) []*protocol.Diagnostic {
	return checkBrackets(tree.Content)
}

// checkBrackets scans source text for mismatched or unclosed brackets, braces, and parens.
func checkBrackets(content string) []*protocol.Diagnostic {
	type stackEntry struct {
		char    rune
		line    uint32
		col     uint32
	}

	openers := map[rune]bool{'(': true, '[': true, '{': true}
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}

	var stack []stackEntry
	var diags []*protocol.Diagnostic
	errSeverity := protocol.SeverityError

	var line, col uint32
	for _, ch := range content {
		if ch == '\n' {
			line++
			col = 0
			continue
		}
		if openers[ch] {
			stack = append(stack, stackEntry{ch, line, col})
		} else if closer, ok := pairs[ch]; ok {
			if len(stack) == 0 || stack[len(stack)-1].char != closer {
				diags = append(diags, &protocol.Diagnostic{
					Range: protocol.Range{
						Start: protocol.Position{Line: line, Character: col},
						End:   protocol.Position{Line: line, Character: col + 1},
					},
					Severity: &errSeverity,
					Source:   _nameKey,
					Message:  fmt.Sprintf("unmatched '%c'", ch),
				})
			} else {
				stack = stack[:len(stack)-1]
			}
		}
		col++
	}

	for _, entry := range stack {
		diags = append(diags, &protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{Line: entry.line, Character: entry.col},
				End:   protocol.Position{Line: entry.line, Character: entry.col + 1},
			},
			Severity: &errSeverity,
			Source:   _nameKey,
			Message:  fmt.Sprintf("unclosed '%c'", entry.char),
		})
	}

	return diags
}
