package treesitter

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/scip-lsp/src/ulsp/controller/diagnostics/diagnosticsmock"
	"github.com/uber/scip-lsp/src/ulsp/entity"
	"github.com/uber/scip-lsp/src/ulsp/repository/session/repositorymock"
	"go.lsp.dev/protocol"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
)

func newTestController(ctrl *gomock.Controller) (*controller, *repositorymock.MockRepository, *diagnosticsmock.MockController) {
	sessions := repositorymock.NewMockRepository(ctrl)
	diags := diagnosticsmock.NewMockController(ctrl)
	c := &controller{
		logger:      zap.NewNop().Sugar(),
		sessions:    sessions,
		diagnostics: diags,
		parseTrees:  make(parseTreeStore),
	}
	return c, sessions, diags
}

func TestNew(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessions := repositorymock.NewMockRepository(ctrl)
	diags := diagnosticsmock.NewMockController(ctrl)

	assert.NotPanics(t, func() {
		New(Params{
			Logger:      zap.NewNop().Sugar(),
			Sessions:    sessions,
			Diagnostics: diags,
		})
	})
}

func TestStartupInfo(t *testing.T) {
	ctrl := gomock.NewController(t)
	c, _, _ := newTestController(ctrl)

	info, err := c.StartupInfo(context.Background())

	require.NoError(t, err)
	assert.NoError(t, info.Validate())
	assert.Equal(t, _nameKey, info.NameKey)
	assert.NotNil(t, info.Methods.Initialize)
	assert.NotNil(t, info.Methods.DidOpen)
	assert.NotNil(t, info.Methods.DidChange)
	assert.NotNil(t, info.Methods.DidClose)
	assert.NotNil(t, info.Methods.EndSession)
}

func TestInitialize(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV4())

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, _ := newTestController(ctrl)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(&entity.Session{UUID: sessionID}, nil)

		err := c.initialize(context.Background(), &protocol.InitializeParams{}, &protocol.InitializeResult{})

		require.NoError(t, err)
		assert.NotNil(t, c.parseTrees[sessionID])
	})

	t.Run("session error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, _ := newTestController(ctrl)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(nil, errors.New("session not found"))

		err := c.initialize(context.Background(), &protocol.InitializeParams{}, &protocol.InitializeResult{})

		assert.Error(t, err)
	})
}

func TestDidOpen(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV4())
	const workspaceRoot = "/workspace"
	const docURI = protocol.DocumentURI("file:///workspace/main.go")

	t.Run("no diagnostics for valid brackets", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = make(map[protocol.DocumentURI]*ParseTree)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), workspaceRoot, gomock.Any(),
			gomock.Len(0)).Return(nil)

		err := c.didOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: docURI, Text: "func foo() {}"},
		})

		require.NoError(t, err)
		assert.NotNil(t, c.parseTrees[sessionID][docURI])
	})

	t.Run("reports diagnostics for unmatched bracket", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = make(map[protocol.DocumentURI]*ParseTree)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), workspaceRoot, gomock.Any(),
			gomock.Not(gomock.Len(0))).Return(nil)

		err := c.didOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: docURI, Text: "func foo( {}"},
		})

		require.NoError(t, err)
	})

	t.Run("session error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, _ := newTestController(ctrl)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(nil, errors.New("no session"))

		err := c.didOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: docURI, Text: "x"},
		})

		assert.Error(t, err)
	})

	t.Run("diagnostics publish error is non-fatal", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = make(map[protocol.DocumentURI]*ParseTree)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(errors.New("publish failed"))

		err := c.didOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: docURI, Text: "x"},
		})

		// Publish errors should not be returned to the caller.
		assert.NoError(t, err)
	})
}

func TestDidChange(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV4())
	const workspaceRoot = "/workspace"
	const docURI = protocol.DocumentURI("file:///workspace/main.go")

	t.Run("no-op when no content changes", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, _, _ := newTestController(ctrl)

		err := c.didChange(context.Background(), &protocol.DidChangeTextDocumentParams{
			TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: docURI}},
			ContentChanges: nil,
		})

		assert.NoError(t, err)
	})

	t.Run("updates parse tree and reports diagnostics", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = make(map[protocol.DocumentURI]*ParseTree)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), workspaceRoot, gomock.Any(),
			gomock.Len(0)).Return(nil)

		err := c.didChange(context.Background(), &protocol.DidChangeTextDocumentParams{
			TextDocument: protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: docURI}},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{
				{Text: "func bar() {}"},
			},
		})

		require.NoError(t, err)
		require.NotNil(t, c.parseTrees[sessionID][docURI])
		assert.Equal(t, "func bar() {}", c.parseTrees[sessionID][docURI].Content)
	})

	t.Run("session error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, _ := newTestController(ctrl)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(nil, errors.New("no session"))

		err := c.didChange(context.Background(), &protocol.DidChangeTextDocumentParams{
			TextDocument: protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: docURI}},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{
				{Text: "x"},
			},
		})

		assert.Error(t, err)
	})
}

func TestDidClose(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV4())
	const workspaceRoot = "/workspace"
	const docURI = protocol.DocumentURI("file:///workspace/main.go")

	t.Run("removes parse tree and clears diagnostics", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = map[protocol.DocumentURI]*ParseTree{
			docURI: {URI: docURI, Content: "x"},
		}

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), workspaceRoot, gomock.Any(),
			gomock.Nil()).Return(nil)

		err := c.didClose(context.Background(), &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		})

		require.NoError(t, err)
		_, exists := c.parseTrees[sessionID][docURI]
		assert.False(t, exists)
	})

	t.Run("session error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, _ := newTestController(ctrl)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(nil, errors.New("no session"))

		err := c.didClose(context.Background(), &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		})

		assert.Error(t, err)
	})

	t.Run("clear diagnostics error is non-fatal", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, sessions, diags := newTestController(ctrl)
		c.parseTrees[sessionID] = make(map[protocol.DocumentURI]*ParseTree)

		sessions.EXPECT().GetFromContext(gomock.Any()).Return(
			&entity.Session{UUID: sessionID, WorkspaceRoot: workspaceRoot}, nil)
		diags.EXPECT().ApplyDiagnostics(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(errors.New("clear failed"))

		err := c.didClose(context.Background(), &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		})

		assert.NoError(t, err)
	})
}

func TestEndSession(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV4())
	const docURI = protocol.DocumentURI("file:///workspace/main.go")

	t.Run("removes all parse trees for session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, _, _ := newTestController(ctrl)
		c.parseTrees[sessionID] = map[protocol.DocumentURI]*ParseTree{
			docURI: {URI: docURI, Content: "x"},
		}

		err := c.endSession(context.Background(), sessionID)

		require.NoError(t, err)
		_, exists := c.parseTrees[sessionID]
		assert.False(t, exists)
	})

	t.Run("no-op for unknown session", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c, _, _ := newTestController(ctrl)

		err := c.endSession(context.Background(), sessionID)

		assert.NoError(t, err)
	})
}
