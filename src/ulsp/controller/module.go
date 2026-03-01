package controller

import (
	"github.com/uber/scip-lsp/src/ulsp/controller/diagnostics"
	docsync "github.com/uber/scip-lsp/src/ulsp/controller/doc-sync"
	"github.com/uber/scip-lsp/src/ulsp/controller/indexer"
	"github.com/uber/scip-lsp/src/ulsp/controller/jdk"
	quickactions "github.com/uber/scip-lsp/src/ulsp/controller/quick-actions"
	scalaassist "github.com/uber/scip-lsp/src/ulsp/controller/scala-assist"
	"github.com/uber/scip-lsp/src/ulsp/controller/scip"
	treesitter "github.com/uber/scip-lsp/src/ulsp/controller/tree-sitter"
	ulspdaemon "github.com/uber/scip-lsp/src/ulsp/controller/ulsp-daemon"
	userguidance "github.com/uber/scip-lsp/src/ulsp/controller/user-guidance"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(ulspdaemon.New),
	fx.Provide(diagnostics.New),
	fx.Provide(docsync.New),
	fx.Provide(quickactions.New),
	fx.Provide(scip.New),
	fx.Provide(userguidance.New),
	fx.Provide(jdk.New),
	fx.Provide(indexer.New),
	fx.Provide(scalaassist.New),
	fx.Provide(treesitter.New),
)
