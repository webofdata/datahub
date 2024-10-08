// Copyright 2021 MIMIRO AS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datahub

import (
	"context"
	"fmt"
	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/labstack/echo/v4"
	"github.com/mimiro-io/datahub/internal/conf"
	"github.com/mimiro-io/datahub/internal/content"
	"github.com/mimiro-io/datahub/internal/jobs"
	"github.com/mimiro-io/datahub/internal/security"
	"github.com/mimiro-io/datahub/internal/server"
	"github.com/mimiro-io/datahub/internal/service/scheduler"
	"github.com/mimiro-io/datahub/internal/web"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"syscall"
)

type DatahubInstance struct {
	config              *conf.Config
	metricsClient       statsd.ClientInterface
	logger              *zap.SugaredLogger
	eventBus            server.EventBus
	store               *server.Store
	dsManager           *server.DsManager
	providerManager     *security.ProviderManager
	tokenProviders      *security.TokenProviders
	runner              *jobs.Runner
	scheduler           *jobs.Scheduler
	contentService      *content.Service
	authorizer          func(logger *zap.SugaredLogger, scopes ...string) echo.MiddlewareFunc
	webService          *web.WebService
	middleware          *web.Middleware
	securityServiceCore *security.ServiceCore
	gc                  *server.GarbageCollector
	backup              *server.BackupManager
	updater             *scheduler.Scheduler
}

func (dhi *DatahubInstance) Start() error {
	dhi.logger.Info("Starting data hub instance")

	dhi.updater.Start()
	// start web server
	go func() {
		err := dhi.webService.Start(context.Background())
		if err != nil {
			dhi.logger.Fatal(err)
		}
	}()

	dhi.waitForStop()

	return nil
}

func LoadConfig(configLocation string) (*conf.Config, error) {
	return conf.LoadConfig(configLocation)
}

func Run(env *conf.Config) {
	dhi, err := NewDatahubInstance(env)
	if err != nil {
		fmt.Println("error initialising data hub " + err.Error())
		return
	}
	err = dhi.Start()
	if err != nil {
		fmt.Println("error starting data hub " + err.Error())
	}
}

func (dhi *DatahubInstance) Stop(ctx context.Context) error {
	dhi.logger.Info("Data hub stopping")
	dhi.webService.Stop(ctx)
	dhi.updater.Stop(ctx)
	dhi.scheduler.Stop(ctx)
	dhi.store.Close()

	return nil
}

func (dhi *DatahubInstance) waitForStop() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan
	dhi.logger.Info("Data hub stopping")
	shutdownCtx := context.Background()
	dhi.Stop(shutdownCtx)
	dhi.logger.Info("Data hub stopped")
	os.Exit(0)
}

func NewDatahubInstance(config *conf.Config) (*DatahubInstance, error) {
	dhi := &DatahubInstance{}
	var err error

	dhi.config = config
	dhi.logger = conf.NewLogger(dhi.config)

	dhi.metricsClient, err = conf.NewMetricsClient(dhi.config, dhi.logger)
	if err != nil {
		return nil, err
	}

	dhi.eventBus, err = server.NewBus(dhi.config)

	// create store and add it to services
	dhi.store = server.NewStore(dhi.config, dhi.metricsClient)
	dhi.dsManager = server.NewDsManager(dhi.config, dhi.store, dhi.eventBus)

	dhi.providerManager = security.NewProviderManager(dhi.config, dhi.store, dhi.logger)
	dhi.securityServiceCore = security.NewServiceCore(dhi.config)
	dhi.tokenProviders = security.NewTokenProviders(dhi.logger, dhi.providerManager, dhi.securityServiceCore)
	dhi.runner = jobs.NewRunner(dhi.config, dhi.store, dhi.tokenProviders, dhi.eventBus, dhi.metricsClient)
	dhi.scheduler = jobs.NewScheduler(dhi.config, dhi.store, dhi.dsManager, dhi.runner)

	dhi.contentService = content.NewContentService(dhi.config, dhi.store, dhi.metricsClient)
	dhi.authorizer = web.NewAuthorizer(dhi.config, dhi.logger, dhi.securityServiceCore)

	// other core services
	conf.InitNewMemoryReporter(dhi.metricsClient, dhi.logger)
	dhi.backup, err = server.NewBackupManager(dhi.store, dhi.config)
	if err != nil {
		return nil, err
	}

	dhi.gc = server.NewGarbageCollector(dhi.store, dhi.config)
	dhi.updater = scheduler.NewScheduler(
		dhi.logger,
		dhi.gc,
		server.NewBadgerAccess(dhi.store, dhi.dsManager),
	)
	// dhi.gc.Start(context.Background())

	// web service config from dhi (ideally we pass through the dhi here or interface)
	// this approach avoids an import loop. which can also be solved by moving some code around
	serviceContext := &web.ServiceContext{}
	serviceContext.Env = dhi.config
	serviceContext.ContentService = dhi.contentService
	serviceContext.Logger = dhi.logger
	serviceContext.Statsd = dhi.metricsClient
	serviceContext.SecurityCore = dhi.securityServiceCore
	serviceContext.JobsScheduler = dhi.scheduler
	serviceContext.DatasetManager = dhi.dsManager
	serviceContext.EventBus = dhi.eventBus
	serviceContext.Port = dhi.config.Port
	serviceContext.TokenProviders = dhi.tokenProviders
	serviceContext.Store = dhi.store

	dhi.webService, err = web.NewWebService(serviceContext)

	// start services
	return dhi, nil
}

/*func wire() *fx.App {
	fxTimeout := 10 * time.Minute
	// set STARTUP_TIMEOUT=120s to override the default timeout
	override, found := os.LookupEnv("STARTUP_TIMEOUT")
	if found {
		d, err := time.ParseDuration(override)
		if err == nil {
			fxTimeout = d
		}
	}
	return fx.New(
		fx.Options(
			fx.StartTimeout(fxTimeout),
		),
		fx.Provide(
			conf.LoadConfig,
			conf.NewMetricsClient,
			conf.NewLogger,
			server.NewBus,
			server.NewStore,
			server.NewDsManager,
			security.NewProviderManager,
			security.NewTokenProviders,
			jobs.NewRunner,
			jobs.NewScheduler,
			content.NewContent,
			web.NewAuthorizer,
			web.NewWebService,
			web.NewMiddleware,
			security.NewServiceCore,
		),
		fx.Invoke( // no other functions are using these, so they need to be invoked to kick things off
			conf.NewMemoryReporter,
			// web.Register,
			web.NewContentHandler,
			web.NewDatasetHandler,
			web.NewTxnHandler,
			web.NewQueryHandler,
			web.NewJobOperationHandler,
			web.NewJobsHandler,
			web.NewNamespaceHandler,
			web.NewProviderHandler,
			server.NewBackupManager,
			server.NewGarbageCollector,
			web.NewSecurityHandler,
		),
	)
}


func Start(ctx context.Context) (*fx.App, error) {
	app := wire()
	err := app.Start(ctx)
	return app, err
} */
