package main

// Helm install/upgrade helpers, aligned with odigos cli/pkg/helm/actions.go
// (kept in-repo so the installer does not depend on a cli pseudo-version that embeds charts
// or on monorepo submodules that are not published as stable Go modules.)

import (
	"errors"
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
)

const helmReleaseName = "odigos"

type installOrUpgradeResult struct {
	Release   *release.Release
	Installed bool
}

type installOrUpgradeOptions struct {
	CreateNamespace      bool
	ResetThenReuseValues bool
}

func installOrUpgradeHelm(actionConfig *action.Configuration, ch *chart.Chart, vals map[string]interface{}, namespace, releaseName string, opts installOrUpgradeOptions) (*installOrUpgradeResult, error) {
	get := action.NewGet(actionConfig)
	_, getErr := get.Run(releaseName)
	if getErr != nil {
		if errors.Is(getErr, driver.ErrReleaseNotFound) {
			rel, err := runInstall(actionConfig, ch, vals, namespace, releaseName, opts.CreateNamespace)
			if err != nil {
				return nil, err
			}
			return &installOrUpgradeResult{Release: rel, Installed: true}, nil
		}
		return nil, getErr
	}

	rel, err := runUpgrade(actionConfig, ch, vals, namespace, releaseName, opts.ResetThenReuseValues)
	if err != nil {
		return nil, err
	}
	return &installOrUpgradeResult{Release: rel, Installed: false}, nil
}

func runInstall(actionConfig *action.Configuration, ch *chart.Chart, vals map[string]interface{}, namespace, releaseName string, createNamespace bool) (*release.Release, error) {
	install := action.NewInstall(actionConfig)
	install.ReleaseName = releaseName
	install.Namespace = namespace
	install.CreateNamespace = createNamespace
	install.ChartPathOptions.Version = ch.Metadata.Version
	return install.Run(ch, vals)
}

func runUpgrade(actionConfig *action.Configuration, ch *chart.Chart, vals map[string]interface{}, namespace, releaseName string, resetThenReuseValues bool) (*release.Release, error) {
	upgrade := action.NewUpgrade(actionConfig)
	upgrade.Namespace = namespace
	upgrade.Install = false
	upgrade.ChartPathOptions.Version = ch.Metadata.Version
	upgrade.ResetThenReuseValues = resetThenReuseValues
	return upgrade.Run(releaseName, ch, vals)
}

func formatInstallOutcome(result *installOrUpgradeResult, chartVer string) string {
	if result.Installed {
		return fmt.Sprintf("Installed release %q in namespace %q (chart version: %s)",
			result.Release.Name, result.Release.Namespace, chartVer)
	}
	return fmt.Sprintf("Upgraded release %q in namespace %q (chart version: %s)",
		result.Release.Name, result.Release.Namespace, chartVer)
}

// runHelmUninstall removes the Helm release (aligned with odigos operator helm.RunUninstall).
func runHelmUninstall(actionConfig *action.Configuration, releaseName string) error {
	uninstall := action.NewUninstall(actionConfig)
	res, err := uninstall.Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			fmt.Printf("Helm release %q not found, treating as already uninstalled\n", releaseName)
			return nil
		}
		return err
	}
	if res == nil {
		fmt.Printf("Helm release %q not found, treating as already uninstalled\n", releaseName)
		return nil
	}
	fmt.Printf("Helm uninstall completed for release %q\n", releaseName)
	return nil
}
