package treesitter

import (
	"context"
	"sync"

	"github.com/gofrs/uuid"
	ulspplugin "github.com/uber/scip-lsp/src/ulsp/entity/ulsp-plugin"
	"github.com/uber/scip-lsp/src/ulsp/repository/session"
	"go.lsp.dev/protocol"
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

	Logger   *zap.SugaredLogger
	Sessions session.Repository
}

type controller struct {
	logger     *zap.SugaredLogger
	sessions   session.Repository
	parseTrees parseTreeStore
	mu         sync.RWMutex
}

// New creates a new tree-sitter controller.
func New(p Params) Controller {
	return &controller{
		logger:     p.Logger.With("plugin", _nameKey),
		sessions:   p.Sessions,
		parseTrees: make(parseTreeStore),
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

// didOpen parses a newly opened document.
func (c *controller) didOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	tree := c.parse(params.TextDocument.URI, params.TextDocument.Text)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parseTrees[s.UUID] == nil {
		return nil
	}
	c.parseTrees[s.UUID][params.TextDocument.URI] = tree
	c.logger.Debugw("parsed document", "uri", params.TextDocument.URI)
	return nil
}

// didChange re-parses a document after it has changed.
func (c *controller) didChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	if len(params.ContentChanges) == 0 {
		return nil
	}

	// Use the last content change as the full document text.
	text := params.ContentChanges[len(params.ContentChanges)-1].Text
	tree := c.parse(params.TextDocument.URI, text)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parseTrees[s.UUID] == nil {
		return nil
	}
	c.parseTrees[s.UUID][params.TextDocument.URI] = tree
	c.logger.Debugw("re-parsed document", "uri", params.TextDocument.URI)
	return nil
}

// didClose removes the parse tree for a closed document.
func (c *controller) didClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parseTrees[s.UUID] != nil {
		delete(c.parseTrees[s.UUID], params.TextDocument.URI)
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
func (c *controller) parse(uri protocol.DocumentURI, content string) *ParseTree {
	return &ParseTree{
		URI:     uri,
		Content: content,
	}
}
