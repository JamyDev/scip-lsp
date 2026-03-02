package treesitter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/scala"
	tstsx "github.com/smacker/go-tree-sitter/typescript/tsx"
	tsts "github.com/smacker/go-tree-sitter/typescript/typescript"

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

// _languageMap maps LSP language IDs to tree-sitter grammars.
var _languageMap = map[string]*sitter.Language{
	"go":              golang.GetLanguage(),
	"java":            java.GetLanguage(),
	"javascript":      javascript.GetLanguage(),
	"javascriptreact": javascript.GetLanguage(),
	"kotlin":          kotlin.GetLanguage(),
	"python":          python.GetLanguage(),
	"scala":           scala.GetLanguage(),
	"typescript":      tsts.GetLanguage(),
	"typescriptreact": tstsx.GetLanguage(),
}

// _extToLangID maps file extensions to LSP language IDs.
var _extToLangID = map[string]string{
	".go":    "go",
	".java":  "java",
	".js":    "javascript",
	".jsx":   "javascriptreact",
	".kt":    "kotlin",
	".kts":   "kotlin",
	".py":    "python",
	".scala": "scala",
	".ts":    "typescript",
	".tsx":   "typescriptreact",
}

// Controller is the interface for the tree-sitter plugin.
type Controller interface {
	StartupInfo(ctx context.Context) (ulspplugin.PluginInfo, error)
}

// ParseTree represents a parsed syntax tree for a document.
type ParseTree struct {
	URI        protocol.DocumentURI
	Content    string
	LanguageID string
	// Tree is the tree-sitter parse tree. Nil for unsupported languages.
	Tree *sitter.Tree
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
	tree := c.parse(ctx, docURI, string(params.TextDocument.LanguageID), params.TextDocument.Text)

	c.mu.Lock()
	if c.parseTrees[s.UUID] != nil {
		c.parseTrees[s.UUID][docURI] = tree
	}
	c.mu.Unlock()

	diags := syntaxDiagnostics(tree)
	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), diags); err != nil {
		c.logger.Warnw("failed to publish diagnostics", "uri", docURI, "error", err)
	}
	c.logger.Debugw("parsed document", "uri", docURI, "language", tree.LanguageID, "diagnostics", len(diags))
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

	// Retrieve the language ID stored during didOpen.
	var languageID string
	c.mu.RLock()
	if existing := c.parseTrees[s.UUID][docURI]; existing != nil {
		languageID = existing.LanguageID
	}
	c.mu.RUnlock()

	text := params.ContentChanges[len(params.ContentChanges)-1].Text
	tree := c.parse(ctx, docURI, languageID, text)

	c.mu.Lock()
	if c.parseTrees[s.UUID] != nil {
		c.parseTrees[s.UUID][docURI] = tree
	}
	c.mu.Unlock()

	diags := syntaxDiagnostics(tree)
	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), diags); err != nil {
		c.logger.Warnw("failed to publish diagnostics", "uri", docURI, "error", err)
	}
	c.logger.Debugw("re-parsed document", "uri", docURI, "language", tree.LanguageID, "diagnostics", len(diags))
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
	delete(c.parseTrees[s.UUID], docURI)
	c.mu.Unlock()

	if err := c.diagnostics.ApplyDiagnostics(ctx, s.WorkspaceRoot, uri.URI(docURI), nil); err != nil {
		c.logger.Warnw("failed to clear diagnostics", "uri", docURI, "error", err)
	}
	return nil
}

func (c *controller) endSession(_ context.Context, sessionID uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.parseTrees, sessionID)
	return nil
}

// parse parses content using the tree-sitter grammar for the given language.
// languageID is the LSP language identifier (e.g. "go", "java"). If empty or
// unknown, the grammar is inferred from the file extension in docURI. Returns a
// ParseTree whose Tree field is nil for unsupported languages, which causes
// syntaxDiagnostics to fall back to bracket matching.
func (c *controller) parse(ctx context.Context, docURI protocol.DocumentURI, languageID, content string) *ParseTree {
	lang, resolvedID := resolveLanguage(languageID, docURI)
	pt := &ParseTree{URI: docURI, Content: content, LanguageID: resolvedID}
	if lang == nil {
		return pt
	}

	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(ctx, nil, []byte(content))
	if err != nil {
		c.logger.Warnw("tree-sitter parse failed", "uri", docURI, "error", err)
		return pt
	}
	pt.Tree = tree
	return pt
}

// resolveLanguage returns the tree-sitter Language and the resolved language ID
// for a given LSP language ID and document URI. Falls back to file-extension
// detection when languageID is empty or unknown.
func resolveLanguage(languageID string, docURI protocol.DocumentURI) (*sitter.Language, string) {
	if lang, ok := _languageMap[languageID]; ok {
		return lang, languageID
	}
	ext := strings.ToLower(filepath.Ext(string(docURI)))
	if id, ok := _extToLangID[ext]; ok {
		return _languageMap[id], id
	}
	return nil, languageID
}

// syntaxDiagnostics walks the tree-sitter AST for ERROR/MISSING nodes.
// Returns nil for unsupported languages (no tree available).
func syntaxDiagnostics(tree *ParseTree) []*protocol.Diagnostic {
	if tree.Tree == nil {
		return nil
	}
	return collectErrors(tree.Tree.RootNode())
}

// collectErrors does a depth-first walk of the AST collecting ERROR and MISSING
// nodes as diagnostics. Recursion stops at ERROR nodes to avoid duplicates.
func collectErrors(root *sitter.Node) []*protocol.Diagnostic {
	var diags []*protocol.Diagnostic

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || n.IsNull() {
			return
		}
		if n.IsError() {
			diags = append(diags, &protocol.Diagnostic{
				Range:    nodeRange(n),
				Severity: protocol.DiagnosticSeverityError,
				Source:   _nameKey,
				Message:  "syntax error",
			})
			// Don't descend — child errors are part of this error region.
			return
		}
		if n.IsMissing() {
			diags = append(diags, &protocol.Diagnostic{
				Range:    nodeRange(n),
				Severity: protocol.DiagnosticSeverityError,
				Source:   _nameKey,
				Message:  fmt.Sprintf("missing %s", n.Type()),
			})
			return
		}
		// Only recurse into subtrees that contain errors.
		for i := 0; i < int(n.ChildCount()); i++ {
			child := n.Child(i)
			if child.HasError() {
				walk(child)
			}
		}
	}

	walk(root)
	return diags
}

// nodeRange converts tree-sitter start/end points to an LSP Range.
func nodeRange(n *sitter.Node) protocol.Range {
	start := n.StartPoint()
	end := n.EndPoint()
	return protocol.Range{
		Start: protocol.Position{Line: start.Row, Character: start.Column},
		End:   protocol.Position{Line: end.Row, Character: end.Column},
	}
}

