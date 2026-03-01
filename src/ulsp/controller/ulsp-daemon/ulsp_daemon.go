// Package ulspdaemon implements the ulsp-daemon business logic.
package ulspdaemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	diagnostics "github.com/uber/scip-lsp/src/ulsp/controller/diagnostics"
	docsync "github.com/uber/scip-lsp/src/ulsp/controller/doc-sync"
	"github.com/uber/scip-lsp/src/ulsp/controller/indexer"
	"github.com/uber/scip-lsp/src/ulsp/controller/jdk"
	quickactions "github.com/uber/scip-lsp/src/ulsp/controller/quick-actions"
	scalaassist "github.com/uber/scip-lsp/src/ulsp/controller/scala-assist"
	"github.com/uber/scip-lsp/src/ulsp/controller/scip"
	treesitter "github.com/uber/scip-lsp/src/ulsp/controller/tree-sitter"
	userguidance "github.com/uber/scip-lsp/src/ulsp/controller/user-guidance"
	"github.com/uber/scip-lsp/src/ulsp/entity"
	ulspplugin "github.com/uber/scip-lsp/src/ulsp/entity/ulsp-plugin"
	ideclient "github.com/uber/scip-lsp/src/ulsp/gateway/ide-client"
	"github.com/uber/scip-lsp/src/ulsp/internal/executor"
	"github.com/uber/scip-lsp/src/ulsp/internal/fs"
	workspaceutils "github.com/uber/scip-lsp/src/ulsp/internal/workspace-utils"
	"github.com/uber/scip-lsp/src/ulsp/mapper"
	"github.com/uber/scip-lsp/src/ulsp/repository/session"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.uber.org/config"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	// Error templates
	_errBadPluginCall       = "calling plugin: %s"
	_errPluginReturnedError = "plugin %q returned error: %s"

	// Configuration keys
	_idleTimeoutMinutesKey = "idleTimeoutMinutes"
	_pluginsKey            = "ulspPlugins"

	// Numerical constants
	// Increasing it to 3 hrs for cursor evaluation
	// TODO: Remove after cursor evaluation
	_contextTimeoutSecondsAsync = 10800
)

// Controller orchestrates the business logic for each request.
type Controller interface {
	// LSP Methods defined per protocol.
	Initialize(ctx context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error)
	Initialized(ctx context.Context, params *protocol.InitializedParams) (err error)
	Shutdown(ctx context.Context) (err error)
	Exit(ctx context.Context) error

	// Document related methods.
	DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error
	DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error
	DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error
	DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error
	WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error
	WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error)
	DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) error
	WillRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error)
	DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) error
	WillCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error)
	DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) error
	WillDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error)
	DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) error

	// Codeintel related methods.
	CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error)
	CodeLens(ctx context.Context, params *protocol.CodeLensParams) (result []protocol.CodeLens, err error)
	CodeLensRefresh(ctx context.Context) (err error)
	CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (result *protocol.CodeLens, err error)

	GotoDeclaration(ctx context.Context, params *protocol.DeclarationParams) (result []protocol.LocationLink, err error)
	GotoDefinition(ctx context.Context, params *protocol.DefinitionParams) (result []protocol.LocationLink, err error)
	GotoTypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) (result []protocol.LocationLink, err error)
	GotoImplementation(ctx context.Context, params *protocol.ImplementationParams) (result []protocol.LocationLink, err error)
	References(ctx context.Context, params *protocol.ReferenceParams) (result []protocol.Location, err error)
	Hover(ctx context.Context, params *protocol.HoverParams) (result *protocol.Hover, err error)
	DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) (result []protocol.DocumentSymbol, err error)

	// Workspace related methods.
	ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error)

	// Window related methods.
	WorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error

	// Custom methods for use within this service.
	RequestFullShutdown(ctx context.Context) error
	InitSession(ctx context.Context, conn *jsonrpc2.Conn) (uuid.UUID, error)
	EndSession(ctx context.Context, uuid uuid.UUID) error
}

// Params are inbound parameters to initialize a new controller.
type Params struct {
	fx.In

	Shutdowner     fx.Shutdowner
	Sessions       session.Repository
	IdeGateway     ideclient.Gateway
	Logger         *zap.SugaredLogger
	Config         config.Provider
	FS             fs.UlspFS
	Executor       executor.Executor
	WorkspaceUtils workspaceutils.WorkspaceUtils

	PluginDiagnostics  diagnostics.Controller
	PluginDocSync      docsync.Controller
	PluginQuickActions quickactions.Controller
	PluginScip         scip.Controller
	PluginUserGuidance userguidance.Controller
	PluginJDK          jdk.Controller
	PluginIndexer      indexer.Controller
	PluginScalaAssist  scalaassist.Controller
	PluginTreeSitter   treesitter.Controller
}

type controller struct {
	sessions           session.Repository
	shutdowner         fx.Shutdowner
	fullShutdown       bool
	idleTimer          *time.Timer
	idleTimerMu        sync.Mutex
	idleTimeoutMinutes time.Duration
	logger             *zap.SugaredLogger
	ideGateway         ideclient.Gateway
	pluginMethods      map[uuid.UUID]ulspplugin.RuntimePrioritizedMethods
	pluginConfig       map[string]bool
	pluginsAll         []ulspplugin.Plugin
	wg                 sync.WaitGroup
	fs                 fs.UlspFS
	executor           executor.Executor
	workspaceUtils     workspaceutils.WorkspaceUtils
}

// New constructs a new top-level controller for the service.
func New(p Params) (Controller, error) {
	ctx := context.Background()

	var timeoutMinutesRaw int64
	if err := p.Config.Get(_idleTimeoutMinutesKey).Populate(&timeoutMinutesRaw); err != nil || timeoutMinutesRaw == 0 {
		return nil, fmt.Errorf("unable to get idle timeout from config: %w", err)
	}
	var pluginConfig map[string]bool
	if err := p.Config.Get(_pluginsKey).Populate(&pluginConfig); err != nil {
		return nil, fmt.Errorf("unable to get plugin keys from config: %w", err)
	}

	// When creating a new plugin, add it as a dependency in Params, then add it to the list of available plugins here.
	availablePlugins := []ulspplugin.Plugin{p.PluginDiagnostics, p.PluginDocSync, p.PluginQuickActions, p.PluginScip, p.PluginUserGuidance, p.PluginJDK, p.PluginIndexer, p.PluginScalaAssist, p.PluginTreeSitter}

	c := &controller{
		sessions:       p.Sessions,
		shutdowner:     p.Shutdowner,
		logger:         p.Logger,
		ideGateway:     p.IdeGateway,
		fs:             p.FS,
		executor:       p.Executor,
		workspaceUtils: p.WorkspaceUtils,

		idleTimeoutMinutes: time.Duration(timeoutMinutesRaw) * time.Minute,
		pluginMethods:      map[uuid.UUID]ulspplugin.RuntimePrioritizedMethods{},
		pluginConfig:       pluginConfig,
		pluginsAll:         availablePlugins,
	}
	c.refreshIdleTimer(ctx)

	return c, nil
}

func (c *controller) registerSessionPlugins(ctx context.Context) error {
	s, err := c.sessions.GetFromContext(ctx)
	if err != nil {
		return fmt.Errorf("getting session from context: %w", err)
	}

	enabledPlugins := []ulspplugin.PluginInfo{}
	for _, plugin := range c.pluginsAll {
		if plugin == nil {
			continue
		}
		info, err := plugin.StartupInfo(ctx)
		if err != nil {
			return fmt.Errorf("getting plugin startup info: %w", err)
		}

		if info.RelevantRepos != nil {
			// Skip if not relevant to this session's repo.
			if _, ok := info.RelevantRepos[s.Monorepo]; !ok {
				continue
			}
		}

		if isEnabled := c.pluginConfig[info.NameKey]; isEnabled {
			c.logger.Infow("plugin registration", "plugin", info.NameKey, "status", "enabled")
			enabledPlugins = append(enabledPlugins, info)
		} else {
			c.logger.Infow("plugin registration", "plugin", info.NameKey, "status", "disabled")
		}
	}
	c.pluginMethods[s.UUID], err = mapper.PluginInfoToRuntimePrioritizedMethods(enabledPlugins)
	if err != nil {
		return fmt.Errorf("prioritizing plugin methods: %w", err)
	}
	return nil
}

// executePluginMethods will execute modules in the order defined for the given method.
// The caller is responsible for defining and providing a handlerSync and handlerAsync function, which should call the corresponding method with proper arguments.
// The same function may be passed in for both sync and async if no difference is needed.
func (c *controller) executePluginMethods(ctx context.Context, method string, handlerSync func(ctx context.Context, m *ulspplugin.Methods), handlerAsync func(ctx context.Context, m *ulspplugin.Methods)) error {
	if handlerSync == nil || handlerAsync == nil {
		return fmt.Errorf("handlers cannot be nil")
	}

	id, err := mapper.ContextToSessionUUID(ctx)
	if err != nil {
		return fmt.Errorf("getting session from context: %w", err)
	}

	if _, ok := c.pluginMethods[id]; !ok {
		return nil
	}

	methodLists, ok := c.pluginMethods[id][method]
	if !ok {
		// No need to execute if this method has no registered plugins.
		return nil
	}

	for _, current := range methodLists.Sync {
		handlerSync(ctx, current)
	}

	// Outer goroutine will spawn a goroutine for each asynchronous plugin method, then wait for them to complete with a timeout.
	// Plugins that implement asynchronous methods are responsible for respecting the context timeout or cancellation signal.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		// New context with its own timeout for asynchronous calls.
		asyncCtx := context.WithValue(context.Background(), entity.SessionContextKey, ctx.Value(entity.SessionContextKey))
		asyncCtx, cancel := context.WithTimeout(asyncCtx, _contextTimeoutSecondsAsync*time.Second)
		defer cancel()

		// Spawn a separate goroutine for each method's context, then wait for them all to complete.
		var innerWg sync.WaitGroup
		for _, current := range methodLists.Async {
			currentMethods := current
			innerWg.Add(1)
			go func() {
				defer innerWg.Done()
				handlerAsync(asyncCtx, currentMethods)
			}()
		}

		innerWg.Wait()
	}()

	return nil
}
