// Copyright 2020 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package setup

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/drone-runners/drone-runner-aws/command/config"
	"github.com/drone-runners/drone-runner-aws/internal/drivers"
	"github.com/drone-runners/drone-runner-aws/internal/lehelper"
	"github.com/drone-runners/drone-runner-aws/internal/poolfile"
	"github.com/drone-runners/drone-runner-aws/store/database"
	"github.com/drone-runners/drone-runner-aws/types"
	"github.com/drone/runner-go/client"
	"github.com/drone/runner-go/logger"
	"github.com/drone/signal"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

// empty context.
var nocontext = context.Background()

type setupCommand struct {
	envFile            string
	vmType             string
	awsAccessKeyID     string
	awsAccessKeySecret string
}

const (
	testPoolName = "test_pool"
)

func (c *setupCommand) run(*kingpin.ParseContext) error {
	// load environment variables from file.
	err := godotenv.Load(c.envFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// load the configuration from the environment
	env, err := config.FromEnviron()
	if err != nil {
		return err
	}
	// map arguments to configuration
	env.AWS.AccessKeyID = c.awsAccessKeyID
	env.AWS.AccessKeySecret = c.awsAccessKeySecret
	// use a single instance db, as we only need one machine
	db, err := database.ProvideDatabase(database.SingleInstance, "")
	if err != nil {
		logrus.WithError(err).
			Fatalln("Invalid or missing hosting provider")
	}
	// setup the global logrus logger.
	setupLogger(&env)

	ctx, cancel := context.WithCancel(nocontext)
	defer cancel()
	// listen for termination signals to gracefully shutdown.
	ctx = signal.WithContextFunc(ctx, func() {
		println("setup: received signal, terminating process")
		cancel()
	})

	cli := client.New(
		env.Client.Address,
		env.Client.Secret,
		env.Client.SkipVerify,
	)
	if env.Client.Dump {
		cli.Dumper = logger.StandardDumper(
			env.Client.DumpBody,
		)
	}
	cli.Logger = logger.Logrus(
		logrus.NewEntry(
			logrus.StandardLogger(),
		),
	)
	// check cli options
	switch c.vmType {
	case string(types.ProviderAmazon):
		logrus.Infoln("setup: using amazon")
		if c.awsAccessKeyID == "" || c.awsAccessKeySecret == "" {
			logrus.Fatalln("missing Amazon access key ID or secret")
		}
	default:
		logrus.Fatalln("unsupported vm provider")
	}
	store := database.ProvideInstanceStore(db)
	poolManager := drivers.New(ctx, store, env.Settings.LiteEnginePath, env.Runner.Name)

	configPool, confErr := poolfile.ConfigPoolFile("", c.vmType, &env)
	if confErr != nil {
		logrus.WithError(err).
			Fatalln("Unable to load pool file, or use an in memory pool")
	}
	// process the pool file
	pools, processErr := poolfile.ProcessPool(configPool, env.Runner.Name)
	if processErr != nil {
		logrus.WithError(processErr).
			Fatalln("setup: unable to process pool file")
	}
	// there is only one instance
	addErr := poolManager.Add(pools[0])
	if addErr != nil {
		logrus.WithError(addErr).
			Fatalln("setup: unable to add pool")
	}
	// provision
	instance, provisionErr := poolManager.Provision(ctx, testPoolName, env.Runner.Name, env.Settings.LiteEnginePath)
	if provisionErr != nil {
		consoleLogs, consoleErr := poolManager.InstanceLogs(ctx, testPoolName, instance.ID)
		logrus.Infof("setup: instance logs for %s: %s", instance.ID, consoleLogs)
		logrus.WithError(provisionErr).
			WithError(consoleErr).
			Fatalln("setup: unable to provision instance")
	}
	// display the console logs
	consoleLogs, err := poolManager.InstanceLogs(ctx, testPoolName, instance.ID)
	if err != nil {
		logrus.WithError(err).
			Errorln("setup: unable to get instance logs")
	}
	logrus.Infof("setup: instance logs for %s: %s", instance.ID, consoleLogs)
	// start the instance
	_, startErr := poolManager.StartInstance(ctx, testPoolName, instance.ID)
	if startErr != nil {
		cleanErr := poolManager.Destroy(ctx, testPoolName, instance.ID)
		consoleLogs, consoleErr := poolManager.InstanceLogs(ctx, testPoolName, instance.ID)
		logrus.Infof("setup: instance logs for %s: %s", instance.ID, consoleLogs)
		logrus.WithError(startErr).
			WithError(cleanErr).
			WithError(consoleErr).
			WithField("instance", instance.ID).
			Fatalln("setup: unable to start instance")
	}
	// create an LE client so we can test the instance
	leClient, leErr := lehelper.GetClient(instance, env.Runner.Name)
	if leErr != nil {
		cleanErr := poolManager.Destroy(ctx, testPoolName, instance.ID)
		consoleLogs, consoleErr := poolManager.InstanceLogs(ctx, testPoolName, instance.ID)
		logrus.Infof("setup: instance logs for %s: %s", instance.ID, consoleLogs)
		logrus.WithError(leErr).
			WithError(cleanErr).
			WithError(consoleErr).
			WithField("instance", instance.ID).
			Fatalln("setup: unable to start lite engine")
	}
	// try the healthcheck api on the lite-engine until it responds ok
	const timeoutSetup = 10 * time.Minute
	logrus.Traceln("setup: running healthcheck and waiting for an ok response")
	healthResponse, healthErr := leClient.RetryHealth(ctx, timeoutSetup)
	if err != nil {
		cleanErr := poolManager.Destroy(ctx, testPoolName, instance.ID)
		logrus.WithError(err).Errorln("failed health check with instance")
		consoleLogs, consoleErr := poolManager.InstanceLogs(ctx, testPoolName, instance.ID)
		logrus.Infof("setup: instance logs for %s: %s", instance.ID, consoleLogs)
		logrus.WithError(healthErr).
			WithError(cleanErr).
			WithError(consoleErr).
			WithField("instance", instance.ID).
			Fatalln("setup: unable to start instance")
	}
	logrus.WithField("response", fmt.Sprintf("%+v", healthResponse)).Traceln("LE.RetryHealth check complete")
	// print the pool file
	marshalledPool, marshalErr := yaml.Marshal(configPool)
	if marshalErr != nil {
		logrus.WithError(marshalErr).
			Errorln("setup: unable to marshal pool file")
	}
	fmt.Printf("setup: everything looks good !\npool file:\n%s", marshalledPool)
	// finally clean the instance
	destroyErr := poolManager.Destroy(ctx, testPoolName, instance.ID)
	return destroyErr
}

func setupLogger(c *config.EnvConfig) {
	logger.Default = logger.Logrus(
		logrus.NewEntry(
			logrus.StandardLogger(),
		),
	)
	if c.Debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	if c.Trace {
		logrus.SetLevel(logrus.TraceLevel)
	}
}

// Register the setup command.
func Register(app *kingpin.Application) {
	c := new(setupCommand)

	cmd := app.Command("setup", "sets up the runner").
		Action(c.run)
	cmd.Flag("type", "which vm provider amazon/anka/google/vmfusion, default is amazon").
		Default(string(types.ProviderAmazon)).
		StringVar(&c.vmType)
	cmd.Flag("envfile", "load the environment variable file").
		Default(".env").
		StringVar(&c.envFile)
	// AWS specific flags
	cmd.Flag("awsAccessKeyID", "aws access key ID").
		Default("").
		StringVar(&c.awsAccessKeyID)
	cmd.Flag("awsAccessKeySecret", "aws access key secret").
		Default("").
		StringVar(&c.awsAccessKeySecret)
}