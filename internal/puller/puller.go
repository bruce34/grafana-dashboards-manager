package puller

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/bruce34/grafana-dashboards-manager/internal/config"
	"github.com/bruce34/grafana-dashboards-manager/internal/git"
	"github.com/bruce34/grafana-dashboards-manager/internal/grafana"

	"github.com/icza/dyno"
	"github.com/sirupsen/logrus"
	gogit "gopkg.in/src-d/go-git.v4"
)

// diffVersion represents a dashboard version diff.
type diffVersion struct {
	old int
	new int
}

func SyncPath(cfg *config.Config) (syncPath string) {
	if cfg.Git != nil {
		syncPath = cfg.Git.ClonePath
	} else {
		syncPath = cfg.SimpleSync.SyncPath
	}
	return
}

func GetGrafanaFileVersion(client *grafana.Client, cfg *config.Config) (dashURIs []string, grafanaVersionFile grafana.VersionFile, err error) {
	// Get URIs for all known dashboards
	logrus.Info("Getting dashboard URIs")
	dashURIs, grafanaDashboardMetaByTitle, grafanaFoldersMetaByUID, _, err := client.GetDashboardsURIs()
	if err != nil {
		return
	}

	grafanaVersionFile = grafana.VersionFile{
		DashboardMetaByTitle:   grafanaDashboardMetaByTitle,
		DashboardMetaBySlug:    make(map[string]grafana.DbSearchResponse, 0),
		DashboardBySlug:        make(map[string]*grafana.Dashboard, 0),
		FoldersMetaByUID:       grafanaFoldersMetaByUID,
		DashboardVersionBySlug: make(map[string]int, 0),
	}
	// Iterate over the dashboards URIs
	for _, uri := range dashURIs {
		logrus.WithFields(logrus.Fields{
			"uri": uri,
		}).Debug("Retrieving dashboard")

		// Retrieve the dashboard JSON
		var dashboard *grafana.Dashboard
		dashboard, err = client.GetDashboard(uri)
		if err != nil {
			return
		}

		if len(cfg.Grafana.IgnorePrefix) > 0 {
			if strings.HasPrefix(dashboard.Slug, cfg.Grafana.IgnorePrefix) {
				logrus.WithFields(logrus.Fields{
					"uri":    uri,
					"name":   dashboard.Name,
					"prefix": cfg.Grafana.IgnorePrefix,
				}).Info("Dashboard name starts with specified prefix, skipping")

				continue
			}
		}

		grafanaVersionFile.DashboardBySlug[dashboard.Slug] = dashboard
		grafanaVersionFile.DashboardMetaBySlug[dashboard.Slug] = grafanaVersionFile.DashboardMetaByTitle[dashboard.Name]
		grafanaVersionFile.DashboardVersionBySlug[dashboard.Slug] = dashboard.Version
	}
	return
}

// PullGrafanaAndCommit pulls all the dashboards from Grafana except the ones
// which name starts with "test", then commits each of them to Git except for
// those that have a newer or equal version number already versioned in the
// repo.
func PullGrafanaAndCommit(client *grafana.Client, cfg *config.Config) (err error) {
	var repo *git.Repository
	var w *gogit.Worktree

	syncPath := SyncPath(cfg)
	// Only do Git stuff if there's a configuration for that. On "simple sync"
	// mode, we don't need to do any versioning.
	// We need to set syncPath accordingly, though, because we use it later.
	if cfg.Git != nil {
		// Clone or pull the repo
		repo, _, err = git.NewRepository(cfg.Git)
		if err != nil {
			return err
		}

		if err = repo.Sync(false); err != nil {
			return err
		}

		w, err = repo.Repo.Worktree()
		if err != nil {
			return err
		}
	}

	var grafanaVersionFile grafana.VersionFile
	_, grafanaVersionFile, err = GetGrafanaFileVersion(client, cfg)
	if err != nil {
		return err
	}

	dv := make(map[string]diffVersion)
	// Load versions
	logrus.Info("PullGrafanaAndCommit: Getting local dashboard versions")
	fileVersionFile, err := GetDashboardsVersions(syncPath, cfg.Git.VersionsFilePrefix)
	if err != nil {
		return err
	}

	// Iterate over the dashboards URIs
	for slug, dashboard := range grafanaVersionFile.DashboardBySlug {
		// Check if there's a version for this dashboard in the data loaded from
		// the "versions.json" file. If there's a version and it's older (lower
		// version number) than the version we just retrieved from the Grafana
		// API, or if there's no known version (ok will be false), write the
		// changes in the repo and add the modified file to the git index.
		fileVersion, ok := fileVersionFile.DashboardVersionBySlug[slug]
		if !ok || dashboard.Version > fileVersion {
			logrus.WithFields(logrus.Fields{
				"slug":          slug,
				"name":          dashboard.Name,
				"local_version": fileVersion,
				"new_version":   dashboard.Version,
			}).Info("Grafana has a newer version, updating")

			if err = addDashboardChangesToRepo(
				dashboard, syncPath, w, grafanaVersionFile.DashboardMetaBySlug[slug].FolderUID,
			); err != nil {
				return err
			}

			// We don't need to check for the value of ok because if ok is false
			// version will be initialised to the 0-value of the int type, which
			// is 0, so the previous version number will be considered to be 0,
			// which is the behaviour we want.
			dv[slug] = diffVersion{
				old: fileVersion,
				new: grafanaVersionFile.DashboardBySlug[slug].Version,
			}
		}
	}

	// remove any dashboards that have gone
	for slug, dashboard := range fileVersionFile.DashboardMetaBySlug {
		logrus.WithFields(logrus.Fields{
			"slug": slug,
			"name": dashboard.Title,
			"got":  grafanaVersionFile.DashboardMetaBySlug[slug],
		}).Debug("dashboard on filesystem")
		if _, ok := grafanaVersionFile.DashboardMetaBySlug[slug]; !ok {
			logrus.WithFields(logrus.Fields{
				"slug": slug,
				"name": dashboard.Title,
			}).Info("Removing dashboard from filesystem")
			removeDashboardFromFilesystem(slug, w)
		}
	}

	// Iterate over the folders
	for _, folderResponse := range grafanaVersionFile.FoldersMetaByUID {
		if err = addFolderChangesToRepo(folderResponse, syncPath, w); err != nil {
			return err
		}
	}

	logrus.WithFields(logrus.Fields{
		"grafanaVersionFile": grafanaVersionFile,
	}).Debug("GrafanaVersionsFile")

	logrus.WithFields(logrus.Fields{
		"fileVersionFile": fileVersionFile,
	}).Debug("FileVersionsFile")

	// Only do Git stuff if there's a configuration for that. On "simple sync"
	// mode, we don't need do do any versioning.
	if cfg.Git != nil {
		// inefficiently, we write the versions here just in case the versions are different but no dashboards are.
		// then the file will be rewritten inside commitNewVersions

		if err = writeVersions(grafanaVersionFile, dv, cfg.Git.ClonePath, cfg.Git.VersionsFilePrefix); err != nil {
			logrus.WithFields(logrus.Fields{
				"err": err,
			}).Info("Marshall error for versions file")
		}

		var status gogit.Status
		status, err = w.Status()
		if err != nil {
			return err
		}

		// Check if there's uncommited changes, and if that's the case, commit
		// them.
		if !cfg.Git.DontCommit {
			if !status.IsClean() {
				logrus.Info("Committing changes")

				if err = commitNewVersions(grafanaVersionFile, dv, w, cfg); err != nil {
					return err
				}
			}
		} else {
			logrus.Info("Skipping git commit - asked not to")
		}

		if !cfg.Git.DontPush && !cfg.Git.DontCommit {
			// Push the changes (we don't do it in the if clause above in case there
			// are pending commits in the local repo that haven't been pushed yet).
			if err = repo.Push(); err != nil {
				logrus.WithFields(logrus.Fields{
					"err": err}).Info("Failed to push")
				return err
			}
		} else {
			logrus.Info("Skipping git commit/push - asked not to")
		}
	} else {
		// If we're on simple sync mode, write versions and don't do anything
		// else.
		if err = writeVersions(grafanaVersionFile, dv, syncPath, cfg.Git.VersionsFilePrefix); err != nil {
			return err
		}
	}

	return nil
}

func addFolderChangesToRepo(
	folderResponse grafana.DbSearchResponse, clonePath string, worktree *gogit.Worktree,
) (err error) {
	folder := grafana.Folder{
		Title:     folderResponse.Title,
		UID:       folderResponse.UID,
		FolderUID: folderResponse.FolderUID,
		URI:       folderResponse.URI,
		Starred:   folderResponse.Starred,
		Tags:      folderResponse.Tags,
	}

	slugExt := folder.Title + ".json"
	dirPath := filepath.Join(clonePath, "folders")
	os.MkdirAll(dirPath, os.ModePerm)
	rawJSON, err := json.Marshal(folder)
	if err != nil {
		return
	}

	if err = rewriteFile(filepath.Join(dirPath, slugExt), rawJSON); err != nil {
		return
	}

	// If worktree is nil, it means that it hasn't been initialised, which means
	// the sync mode is "simple sync" and not Git.
	if worktree != nil {
		if _, err = worktree.Add(filepath.Join("folders", slugExt)); err != nil {
			return err
		}
	}

	return
}

// addDashboardChangesToRepo writes a dashboard content in a file, then adds the
// file to the git index so it can be comitted afterwards.
// Returns an error if there was an issue with either of the steps.
func addDashboardChangesToRepo(
	dashboard *grafana.Dashboard, clonePath string, worktree *gogit.Worktree, folderUID string) error {
	slugExt := dashboard.Slug + ".json"
	// we take out the versions here, as versions are generated by grafana and
	// therefore can't be sanely sync'd across multiple grafana instances
	var jsRaw interface{}
	if err := json.Unmarshal([]byte(dashboard.RawJSON), &jsRaw); err != nil {
		return err
	}
	// the following keys are unique only to an individual grafana instance
	dyno.Delete(jsRaw, "version")
	dyno.Delete(jsRaw, "id")
	dyno.Set(jsRaw, folderUID, "__folderUID")
	rawJSON, err := json.Marshal(jsRaw)
	if err != nil {
		return err
	}

	dirPath := filepath.Join(clonePath, "dashboards")
	os.MkdirAll(dirPath, os.ModePerm)

	if err := rewriteFile(filepath.Join(dirPath, slugExt), rawJSON); err != nil {
		return err
	}

	// If worktree is nil, it means that it hasn't been initialised, which means
	// the sync mode is "simple sync" and not Git.
	if worktree != nil {
		if _, err := worktree.Add(filepath.Join("dashboards", slugExt)); err != nil {
			return err
		}
	}

	return nil
}

func removeDashboardFromFilesystem(slug string, worktree *gogit.Worktree) (err error) {
	_, err = worktree.Remove(filepath.Join("dashboards", slug+".json"))
	return
}

// rewriteFile removes a given file and re-creates it with a new content. The
// content is provided as JSON, and is then indented before being written down.
// We need the whole "remove then recreate" thing because, if the file already
// exists, ioutil.WriteFile will append the content to it. However, we want to
// replace the oldest version with another (so git can diff it), so we re-create
// the file with the changed content.
// Returns an error if there was an issue when removing or writing the file, or
// indenting the JSON content.
func rewriteFile(filename string, content []byte) error {
	if err := os.Remove(filename); err != nil {
		pe, ok := err.(*os.PathError)
		if !ok || pe.Err.Error() != "no such file or directory" {
			return err
		}
	}

	indentedContent, err := indent(content)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filename, indentedContent, 0644)
}

// indent indents a given JSON content with tabs.
// We need to indent the content as the Grafana API returns a one-lined JSON
// string, which isn't great to work with.
// Returns an error if there was an issue with the process.
func indent(srcJSON []byte) (indentedJSON []byte, err error) {
	buf := bytes.NewBuffer(nil)
	if err = json.Indent(buf, srcJSON, "", "\t"); err != nil {
		return
	}

	indentedJSON, err = ioutil.ReadAll(buf)
	return
}
