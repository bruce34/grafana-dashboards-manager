package main

import (
	"flag"
	"github.com/bruce34/grafana-dashboards-manager/internal/puller"
	"os"

	"github.com/bruce34/grafana-dashboards-manager/internal/config"
	"github.com/bruce34/grafana-dashboards-manager/internal/grafana"
	"github.com/bruce34/grafana-dashboards-manager/internal/logger"
	"github.com/bruce34/grafana-dashboards-manager/internal/poller"
	"github.com/bruce34/grafana-dashboards-manager/internal/webhook"

	"github.com/sirupsen/logrus"
)

var (
	deleteRemoved = flag.Bool("delete-removed", false, "For each file removed from Git, delete the corresponding dashboard on the Grafana API")
	pushAll       = flag.Bool("push-all", false, "Force push all files, then quit")
	singleShot    = flag.Bool("single-shot", false, "Run once, then quit")
)

func main() {
	var err error

	// Define this flag in the main function because else it would cause a
	// conflict with the one in the puller.
	configFile := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()

	// Load the logger's configuration.
	logger.LogConfig()

	// Load the configuration.
	cfg, err := config.Load(*configFile)
	if err != nil {
		logrus.Panic(err)
	}

	if cfg.Git == nil || cfg.Pusher == nil {
		logrus.Info("The git configuration or the pusher configuration (or both) is not defined in the configuration file. The pusher cannot start unless both are defined.")
		os.Exit(0)
	}

	// Initialise the Grafana API client.
	grafanaClient := grafana.NewClient(cfg.Grafana.BaseURL, cfg.Grafana.APIKey, cfg.Grafana.Username, cfg.Grafana.Password, cfg.Grafana.SkipVerify)

	if *pushAll {
		syncPath := puller.SyncPath(cfg)
		dashboardFiles, dashboardContents, err := grafana.LoadFilesFromDirectory(cfg, syncPath, "/dashboards")
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Warn("Unable to push all files")
		}
		var fileVersionFile grafana.VersionFile
		fileVersionFile, err = puller.GetDashboardsVersions(syncPath, cfg.Git.VersionsFilePrefix)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Warn("Unable to read dashboard metadata file. Consider copying another hosts if running for the first time?")
		}
		logrus.WithFields(logrus.Fields{
			"dashboardFiles": dashboardFiles,
			//	"dashboardContents": dashboardContents,
			"fileVersionFile": fileVersionFile,
			"error":           err,
		}).Info("About to")

		folderFiles, folderContents, err := grafana.LoadFilesFromDirectory(cfg, syncPath, "/folders")

		// ensure all folders are created before we query for them
		grafanaClient.CreateFolders(folderFiles, folderContents)
		var grafanaVersionFile grafana.VersionFile
		_, grafanaVersionFile, err = puller.GetGrafanaFileVersion(grafanaClient, cfg)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to get grafana meta data")
		}

		grafana.Push(cfg, fileVersionFile, grafanaVersionFile, dashboardFiles, dashboardContents, grafanaClient)

		os.Exit(0)
	}

	// Set up either a webhook or a poller depending on the mode specified in the
	// configuration file.
	switch cfg.Pusher.Mode {
	case "webhook":
		err = webhook.Setup(cfg, grafanaClient, *deleteRemoved)
		break
	case "git-pull":
		err = poller.Setup(cfg, grafanaClient, *deleteRemoved, *singleShot)
	}

	if err != nil {
		logrus.Panic(err)
		os.Exit(1)
	}
}
