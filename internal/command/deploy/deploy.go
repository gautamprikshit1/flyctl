package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/terminal"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/build/imgsrc"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/env"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/render"
	"github.com/superfly/flyctl/internal/sentry"
	"github.com/superfly/flyctl/internal/state"

	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/internal/cmdutil"
	"github.com/superfly/flyctl/internal/logger"
	"github.com/superfly/flyctl/internal/watch"
)

var CommonFlags = flag.Set{
	flag.Region(),
	flag.Image(),
	flag.Now(),
	flag.RemoteOnly(false),
	flag.LocalOnly(),
	flag.Push(),
	flag.Detach(),
	flag.Strategy(),
	flag.Dockerfile(),
	flag.Ignorefile(),
	flag.ImageLabel(),
	flag.BuildArg(),
	flag.BuildSecret(),
	flag.BuildTarget(),
	flag.NoCache(),
	flag.Nixpacks(),
	flag.BuildOnly(),
	flag.StringSlice{
		Name:        "env",
		Shorthand:   "e",
		Description: "Set of environment variables in the form of NAME=VALUE pairs. Can be specified multiple times.",
	},
	flag.Bool{
		Name:        "auto-confirm",
		Description: "Will automatically confirm changes when running non-interactively.",
	},
	flag.Int{
		Name:        "wait-timeout",
		Description: "Seconds to wait for individual machines to transition states and become healthy.",
		Default:     int(DefaultWaitTimeout.Seconds()),
	},
	flag.Int{
		Name:        "lease-timeout",
		Description: "Seconds to lease individual machines while running deployment. All machines are leased at the beginning and released at the end. The lease is refreshed periodically for this same time, which is why it is short. flyctl releases leases in most cases.",
		Default:     int(DefaultLeaseTtl.Seconds()),
	},
	flag.Bool{
		Name:        "force-nomad",
		Description: "Use the Apps v1 platform built with Nomad",
		Default:     false,
	},
	flag.Bool{
		Name:        "force-machines",
		Description: "Use the Apps v2 platform built with Machines",
		Default:     false,
	},
}

func New() (cmd *cobra.Command) {
	const (
		long = `Deploy Fly applications from source or an image using a local or remote builder.

		To disable colorized output and show full Docker build output, set the environment variable NO_COLOR=1.
	`
		short = "Deploy Fly applications"
	)

	cmd = command.New("deploy [WORKING_DIRECTORY]", short, long, run,
		command.RequireSession,
		command.ChangeWorkingDirectoryToFirstArgIfPresent,
		command.RequireAppName,
	)

	cmd.Args = cobra.MaximumNArgs(1)

	flag.Add(cmd,
		CommonFlags,
		flag.App(),
		flag.AppConfig(),
	)

	return
}

func run(ctx context.Context) error {
	appConfig, err := determineAppConfig(ctx)
	if err != nil {
		return err
	}

	return DeployWithConfig(ctx, appConfig, DeployWithConfigArgs{
		ForceNomad:    flag.GetBool(ctx, "force-nomad"),
		ForceMachines: flag.GetBool(ctx, "force-machines"),
		ForceYes:      flag.GetBool(ctx, "auto-confirm"),
	})
}

type DeployWithConfigArgs struct {
	ForceMachines bool
	ForceNomad    bool
	ForceYes      bool
}

func DeployWithConfig(ctx context.Context, appConfig *appconfig.Config, args DeployWithConfigArgs) (err error) {
	apiClient := client.FromContext(ctx).API()
	appNameFromContext := appconfig.NameFromContext(ctx)
	appCompact, err := apiClient.GetAppCompact(ctx, appNameFromContext)
	if err != nil {
		return err
	}
	deployToMachines, err := useMachines(ctx, appConfig, appCompact, args, apiClient)
	if err != nil {
		return err
	}

	if deployToMachines {
		err := appConfig.EnsureV2Config()
		if err != nil {
			return fmt.Errorf("Can't deploy an invalid app config: %s", err)
		}
	}

	// Fetch an image ref or build from source to get the final image reference to deploy
	img, err := determineImage(ctx, appConfig)
	if err != nil {
		return fmt.Errorf("failed to fetch an image or build from source: %w", err)
	}

	if flag.GetBuildOnly(ctx) {
		return nil
	}

	var release *api.Release
	var releaseCommand *api.ReleaseCommand

	// Assign an empty map if nil so later assignments won't fail
	if appConfig.PrimaryRegion != "" && appConfig.Env["PRIMARY_REGION"] == "" {
		appConfig.SetEnvVariable("PRIMARY_REGION", appConfig.PrimaryRegion)
	}

	if deployToMachines {
		primaryRegion := appConfig.PrimaryRegion
		if flag.GetString(ctx, flag.RegionName) != "" {
			primaryRegion = flag.GetString(ctx, flag.RegionName)
		}

		md, err := NewMachineDeployment(ctx, MachineDeploymentArgs{
			AppCompact:        appCompact,
			DeploymentImage:   img,
			Strategy:          flag.GetString(ctx, "strategy"),
			EnvFromFlags:      flag.GetStringSlice(ctx, "env"),
			PrimaryRegionFlag: primaryRegion,
			BuildOnly:         flag.GetBuildOnly(ctx),
			SkipHealthChecks:  flag.GetDetach(ctx),
			WaitTimeout:       time.Duration(flag.GetInt(ctx, "wait-timeout")) * time.Second,
			LeaseTimeout:      time.Duration(flag.GetInt(ctx, "lease-timeout")) * time.Second,
		})
		if err != nil {
			sentry.CaptureExceptionWithAppInfo(err, "deploy", appCompact)
			return err
		}
		err = md.DeployMachinesApp(ctx)
		if err != nil {
			sentry.CaptureExceptionWithAppInfo(err, "deploy", appCompact)
		}
		return err
	}

	release, releaseCommand, err = createRelease(ctx, appConfig, img)
	if err != nil {
		return err
	}

	if flag.GetDetach(ctx) {
		return nil
	}

	// TODO: This is a single message that doesn't belong to any block output, so we should have helpers to allow that
	tb := render.NewTextBlock(ctx)
	tb.Done("You can detach the terminal anytime without stopping the deployment")

	// Run the pre-deployment release command if it's set
	if releaseCommand != nil {
		// TODO: don't use text block here
		tb := render.NewTextBlock(ctx, fmt.Sprintf("Release command detected: %s\n", releaseCommand.Command))
		tb.Done("This release will not be available until the release command succeeds.")

		if err := watch.ReleaseCommand(ctx, appConfig.AppName, releaseCommand.ID); err != nil {
			return err
		}

		release, err = apiClient.GetAppReleaseNomad(ctx, appConfig.AppName, release.ID)
		if err != nil {
			return err
		}
	}

	if release.DeploymentStrategy == "IMMEDIATE" {
		logger := logger.FromContext(ctx)
		logger.Debug("immediate deployment strategy, nothing to monitor")

		return nil
	}

	err = watch.Deployment(ctx, appConfig.AppName, release.EvaluationID)

	return err
}

func useMachines(ctx context.Context, appConfig *appconfig.Config, appCompact *api.AppCompact, args DeployWithConfigArgs, apiClient *api.Client) (bool, error) {
	appsV2DefaultOn, _ := apiClient.GetAppsV2DefaultOnForOrg(ctx, appCompact.Organization.Slug)
	switch {
	case !appCompact.Deployed && args.ForceNomad:
		return false, nil
	case !appCompact.Deployed && args.ForceMachines:
		return true, nil
	case !appCompact.Deployed && appCompact.PlatformVersion == appconfig.MachinesPlatform:
		return true, nil
	case appCompact.Deployed:
		return appCompact.PlatformVersion == appconfig.MachinesPlatform, nil
	case args.ForceYes:
		return appsV2DefaultOn, nil
	default:
		return appsV2DefaultOn, nil
	}
}

// determineAppConfig fetches the app config from a local file, or in its absence, from the API
func determineAppConfig(ctx context.Context) (cfg *appconfig.Config, err error) {
	tb := render.NewTextBlock(ctx, "Verifying app config")
	appNameFromContext := appconfig.NameFromContext(ctx)
	if cfg = appconfig.ConfigFromContext(ctx); cfg == nil {
		logger := logger.FromContext(ctx)
		logger.Debug("no local app config detected; fetching from backend ...")

		var flapsClient *flaps.Client
		flapsClient, err = flaps.NewFromAppName(ctx, appNameFromContext)
		if err != nil {
			return nil, fmt.Errorf("could not create flaps client: %w", err)
		}
		ctx = flaps.NewContext(ctx, flapsClient)

		cfg, err = appconfig.FromRemoteApp(ctx, appNameFromContext)
		if err != nil {
			return
		}

	}

	if env := flag.GetStringSlice(ctx, "env"); len(env) > 0 {
		var parsedEnv map[string]string
		if parsedEnv, err = cmdutil.ParseKVStringsToMap(env); err != nil {
			err = fmt.Errorf("failed parsing environment: %w", err)

			return
		}
		cfg.SetEnvVariables(parsedEnv)
	}

	if regionCode := flag.GetString(ctx, flag.RegionName); regionCode != "" {
		cfg.PrimaryRegion = regionCode
	}

	// Always prefer the app name passed via --app
	if appNameFromContext != "" {
		cfg.AppName = appNameFromContext
	}

	err, extraInfo := cfg.Validate(ctx)
	if extraInfo != "" {
		fmt.Print(extraInfo)
	}
	if err != nil {
		return
	}

	tb.Done("Verified app config")
	return
}

// determineImage picks the deployment strategy, builds the image and returns a
// DeploymentImage struct
func determineImage(ctx context.Context, appConfig *appconfig.Config) (img *imgsrc.DeploymentImage, err error) {
	tb := render.NewTextBlock(ctx, "Building image")
	daemonType := imgsrc.NewDockerDaemonType(!flag.GetRemoteOnly(ctx), !flag.GetLocalOnly(ctx), env.IsCI(), flag.GetBool(ctx, "nixpacks"))

	client := client.FromContext(ctx).API()
	io := iostreams.FromContext(ctx)

	if len(appConfig.BuildStrategies()) > 0 {
		foundDF := imgsrc.ResolveDockerfile(state.WorkingDirectory(ctx))
		configDF, _ := resolveDockerfilePath(ctx, appConfig)
		if foundDF != "" && foundDF != configDF {
			terminal.Warnf("Ignoring %s due to config\n", foundDF)
		}
	}

	resolver := imgsrc.NewResolver(daemonType, client, appConfig.AppName, io)

	var imageRef string
	if imageRef, err = fetchImageRef(ctx, appConfig); err != nil {
		return
	}

	// we're using a pre-built Docker image
	if imageRef != "" {
		opts := imgsrc.RefOptions{
			AppName:    appConfig.AppName,
			WorkingDir: state.WorkingDirectory(ctx),
			Publish:    !flag.GetBuildOnly(ctx),
			ImageRef:   imageRef,
			ImageLabel: flag.GetString(ctx, "image-label"),
		}

		img, err = resolver.ResolveReference(ctx, io, opts)

		return
	}

	build := appConfig.Build
	if build == nil {
		build = new(appconfig.Build)
	}

	// We're building from source
	opts := imgsrc.ImageOptions{
		AppName:         appConfig.AppName,
		WorkingDir:      state.WorkingDirectory(ctx),
		Publish:         flag.GetBool(ctx, "push") || !flag.GetBuildOnly(ctx),
		ImageLabel:      flag.GetString(ctx, "image-label"),
		NoCache:         flag.GetBool(ctx, "no-cache"),
		BuiltIn:         build.Builtin,
		BuiltInSettings: build.Settings,
		Builder:         build.Builder,
		Buildpacks:      build.Buildpacks,
	}

	cliBuildSecrets, err := cmdutil.ParseKVStringsToMap(flag.GetStringSlice(ctx, "build-secret"))
	if err != nil {
		return
	}

	if cliBuildSecrets != nil {
		opts.BuildSecrets = cliBuildSecrets
	}

	var buildArgs map[string]string
	if buildArgs, err = mergeBuildArgs(ctx, build.Args); err != nil {
		return
	}

	opts.BuildArgs = buildArgs

	if opts.DockerfilePath, err = resolveDockerfilePath(ctx, appConfig); err != nil {
		return
	}

	if opts.IgnorefilePath, err = resolveIgnorefilePath(ctx, appConfig); err != nil {
		return
	}

	if target := appConfig.DockerBuildTarget(); target != "" {
		opts.Target = target
	} else if target := flag.GetString(ctx, "build-target"); target != "" {
		opts.Target = target
	}

	// finally, build the image
	heartbeat := resolver.StartHeartbeat(ctx)
	defer resolver.StopHeartbeat(heartbeat)
	if img, err = resolver.BuildImage(ctx, io, opts); err == nil && img == nil {
		err = errors.New("no image specified")
	}

	if err == nil {
		tb.Printf("image: %s\n", img.Tag)
		tb.Printf("image size: %s\n", humanize.Bytes(uint64(img.Size)))
	}

	return
}

// resolveDockerfilePath returns the absolute path to the Dockerfile
// if one was specified in the app config or a command line argument
func resolveDockerfilePath(ctx context.Context, appConfig *appconfig.Config) (path string, err error) {
	defer func() {
		if err == nil && path != "" {
			path, err = filepath.Abs(path)
		}
	}()

	if path = appConfig.Dockerfile(); path != "" {
		path = filepath.Join(filepath.Dir(appConfig.ConfigFilePath()), path)
	} else {
		path = flag.GetString(ctx, "dockerfile")
	}

	return
}

// resolveIgnorefilePath returns the absolute path to the Dockerfile
// if one was specified in the app config or a command line argument
func resolveIgnorefilePath(ctx context.Context, appConfig *appconfig.Config) (path string, err error) {
	defer func() {
		if err == nil && path != "" {
			path, err = filepath.Abs(path)
		}
	}()

	if path = appConfig.Ignorefile(); path != "" {
		path = filepath.Join(filepath.Dir(appConfig.ConfigFilePath()), path)
	} else {
		path = flag.GetString(ctx, "ignorefile")
	}

	return
}

func mergeBuildArgs(ctx context.Context, args map[string]string) (map[string]string, error) {
	if args == nil {
		args = make(map[string]string)
	}

	// set additional Docker build args from the command line, overriding similar ones from the config
	cliBuildArgs, err := cmdutil.ParseKVStringsToMap(flag.GetStringSlice(ctx, "build-arg"))
	if err != nil {
		return nil, fmt.Errorf("invalid build args: %w", err)
	}

	for k, v := range cliBuildArgs {
		args[k] = v
	}
	return args, nil
}

func fetchImageRef(ctx context.Context, cfg *appconfig.Config) (ref string, err error) {
	if ref = flag.GetString(ctx, "image"); ref != "" {
		return
	}

	if cfg != nil && cfg.Build != nil {
		if ref = cfg.Build.Image; ref != "" {
			return
		}
	}

	return ref, nil
}

func createRelease(ctx context.Context, appConfig *appconfig.Config, img *imgsrc.DeploymentImage) (*api.Release, *api.ReleaseCommand, error) {
	tb := render.NewTextBlock(ctx, "Creating release")

	input := api.DeployImageInput{
		AppID: appConfig.AppName,
		Image: img.Tag,
	}

	// Set the deployment strategy
	if val := flag.GetString(ctx, "strategy"); val != "" {
		input.Strategy = api.StringPointer(strings.ReplaceAll(strings.ToUpper(val), "-", "_"))
	}

	input.Definition = api.DefinitionPtr(appConfig.SanitizedDefinition())

	// Start deployment of the determined image
	client := client.FromContext(ctx).API()

	release, releaseCommand, err := client.DeployImage(ctx, input)
	if err == nil {
		tb.Donef("release v%d created\n", release.Version)
	}

	return release, releaseCommand, err
}
