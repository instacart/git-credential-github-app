package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v75/github"
	"github.com/hashicorp/go-retryablehttp"
)

var version = "v0.0.1"

const (
	retryMax     = 4
	retryWaitMin = 1 * time.Second
	retryWaitMax = 30 * time.Second

	// Hard ceilings on total wall-clock time (including retry backoff) so a
	// misbehaving server can never hang git indefinitely. The get path is in
	// git's critical path and kept tighter; generate is an interactive,
	// one-shot command that may paginate, so it gets more headroom.
	getTimeout      = 60 * time.Second
	generateTimeout = 2 * time.Minute
)

type CredHelperArgs struct {
	AppId          int64
	InstallationId int64
	Organization   string
	PrivateKeyFile string
	Username       string
	Domain         string
}

func printVersion(verbose bool) {
	fmt.Fprintln(os.Stderr, "version", version)
	if verbose {
		buildInfo, ok := debug.ReadBuildInfo()
		if !ok {
			log.Fatal("Cannot get build information from binary")
		}
		fmt.Fprintln(os.Stderr, buildInfo.String())
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Git Credential Helper for Github Apps")
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, os.Args[0], "-h|--help")
	fmt.Fprintln(os.Stderr, os.Args[0], "-v|--version")
	fmt.Fprintln(os.Stderr, os.Args[0], "<-username USERNAME> <-appId ID> <-privateKeyFile PATH_TO_PRIVATE_KEY> <[-installationId INSTALLATION_ID] | [-organization ORGANIZATION]> [-domain GHE_DOMAIN] <get|store|erase>")
	fmt.Fprintln(os.Stderr, os.Args[0], "<-username USERNAME> <-appId ID> <-privateKeyFile PATH_TO_PRIVATE_KEY> [-domain GHE_DOMAIN] generate")
	fmt.Fprintln(os.Stderr, "Options:")
	flag.PrintDefaults()
}

func fatal(v ...any) {
	fmt.Println("quit=1")
	log.Fatal(v...)
}

func credentialGetOutput(w io.Writer, username string, token *github.InstallationToken) error {
	_, err := fmt.Fprintf(w, "username=%s\npassword=%s\npassword_expiry_utc=%d\n",
		username,
		token.GetToken(),
		token.GetExpiresAt().Unix())
	return err
}

func generateGitConfig(w io.Writer, installations []*github.Installation, args *CredHelperArgs) {
	domain := "github.com"
	if args.Domain != "" {
		domain = args.Domain
	}

	for _, installation := range installations {
		fmt.Fprintf(w, "[credential \"%s\"]\n\tuseHttpPath = true\n\thelper = \"github-app -username %s -appId %d -privateKeyFile %s -installationId %d\"\n",
			installation.GetAccount().GetHTMLURL(), args.Username, args.AppId, args.PrivateKeyFile, installation.GetID())
	}
	fmt.Fprintf(w, "[credential \"https://%s\"]\n\thelper = \"cache --timeout=43200\"\n", domain)
	fmt.Fprintf(w, "[url \"https://%s\"]\n\tinsteadOf = ssh://git@github.com\n", domain)
}

// newRetryableTransport returns an http.RoundTripper that transparently retries
// requests that fail with transient errors: 5XX server responses (except 501),
// 429 rate limiting (honoring Retry-After), and network-level errors. Retries
// use exponential backoff with jitter and are bounded by RetryMax.
func newRetryableTransport() http.RoundTripper {
	return newRetryableClient().StandardClient().Transport
}

// newRetryableClient builds the retrying HTTP client used by newRetryableTransport.
// It is factored out so tests can adjust the backoff timings.
func newRetryableClient() *retryablehttp.Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = retryMax
	retryClient.RetryWaitMin = retryWaitMin
	retryClient.RetryWaitMax = retryWaitMax
	retryClient.CheckRetry = githubRetryPolicy
	retryClient.Backoff = cappedBackoff
	// Suppress the library's verbose per-request logging; emit a brief warning
	// to stderr only when a request is actually being retried.
	retryClient.Logger = nil
	retryClient.RequestLogHook = func(_ retryablehttp.Logger, req *http.Request, attempt int) {
		if attempt > 0 {
			fmt.Fprintf(os.Stderr, "retrying request (attempt %d/%d): %s %s\n",
				attempt, retryClient.RetryMax, req.Method, req.URL.Path)
		}
	}
	return retryClient
}

// githubRetryPolicy extends retryablehttp's default policy (network errors, 429,
// and 5xx except 501) to also retry GitHub's secondary rate-limit responses,
// which arrive as 403 with a Retry-After header. Primary rate-limit 403s (no
// Retry-After, reset potentially an hour away) are deliberately not retried so
// git fails fast rather than hanging.
func githubRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	shouldRetry, checkErr := retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	if shouldRetry || checkErr != nil {
		return shouldRetry, checkErr
	}
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		if _, ok := resp.Header["Retry-After"]; ok {
			return true, nil
		}
	}
	return false, nil
}

// cappedBackoff honors a server-supplied Retry-After header (seconds) for 403,
// 429, and 503 responses but never waits longer than retryWaitMax, and otherwise
// falls back to exponential backoff. This guarantees a bounded per-attempt wait
// even when a server requests a very long (or HTTP-date) Retry-After delay.
func cappedBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				return capDuration(time.Duration(secs)*time.Second, max)
			}
		}
	}
	// DefaultBackoff returns an uncapped Retry-After for HTTP-date headers, so
	// cap its result too.
	return capDuration(retryablehttp.DefaultBackoff(min, max, attemptNum, resp), max)
}

func capDuration(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

func newGithubAppClient(tr http.RoundTripper, appId int64, privateKeyFile, domain string) (*github.Client, error) {
	atr, err := ghinstallation.NewAppsTransportKeyFromFile(tr, appId, privateKeyFile)
	if err != nil {
		return nil, err
	}

	client := github.NewClient(&http.Client{Transport: atr})
	if domain == "" {
		return client, nil
	}

	baseUrl := "https://" + domain
	atr.BaseURL = baseUrl + "/api/v3"
	// Enterprise URLs need a terminating slash
	return client.WithEnterpriseURLs(baseUrl+"/api/v3/", baseUrl+"/api/uploads/")
}

func doGet(w io.Writer, args *CredHelperArgs) {
	client, err := newGithubAppClient(newRetryableTransport(), args.AppId, args.PrivateKeyFile, args.Domain)
	if err != nil {
		log.Fatal("Error creating client: ", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()

	if args.InstallationId == 0 {
		installation, _, err := client.Apps.FindOrganizationInstallation(ctx, args.Organization)
		if err != nil {
			fatal("Could not get InstallationId from Organization: ", err)
		}
		args.InstallationId = *installation.ID
	}

	installationToken, _, err := client.Apps.CreateInstallationToken(ctx, args.InstallationId, nil)
	if err != nil {
		fatal("Could not create Github App Installation Access Token: ", err)
	}
	credentialGetOutput(w, args.Username, installationToken)
}

func doGenerate(w io.Writer, args *CredHelperArgs) {
	client, err := newGithubAppClient(newRetryableTransport(), args.AppId, args.PrivateKeyFile, args.Domain)
	if err != nil {
		log.Fatal("Error creating client: ", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), generateTimeout)
	defer cancel()

	var allInstallations []*github.Installation
	opt := github.ListOptions{PerPage: 10}
	for {
		installations, resp, err := client.Apps.ListInstallations(ctx, &opt)
		if err != nil {
			log.Fatal("Error retrieving installations: ", err)
		}
		allInstallations = append(allInstallations, installations...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	generateGitConfig(w, allInstallations, args)
}

func main() {
	args := CredHelperArgs{}
	versionFlagPtr := flag.Bool("version", false, "Get application version")
	flag.Int64Var(&args.AppId, "appId", 0, "GitHub App AppId, mandatory")
	flag.Int64Var(&args.InstallationId, "installationId", 0, "GitHub App Installation ID")
	flag.StringVar(&args.Organization, "organization", "", "GitHub App Organization, optional")
	flag.StringVar(&args.PrivateKeyFile, "privateKeyFile", "", "GitHub App Private Key File Path, mandatory")
	flag.StringVar(&args.Username, "username", "", "Git Credential Username, mandatory, recommend GitHub App Name")
	flag.StringVar(&args.Domain, "domain", "", "GitHub Enterprise domain, optional")

	flag.Parse()

	if *versionFlagPtr {
		printVersion(true)
		os.Exit(0)
	}

	if flag.NArg() != 1 {
		printUsage()
		os.Exit(1)
	}

	if args.AppId == 0 {
		log.Fatal("appId is mandatory")
	}

	if len(args.PrivateKeyFile) == 0 {
		log.Fatal("Path to Private Key file is mandatory")
	}

	if len(args.Username) == 0 {
		log.Fatal("username is mandatory")
	}

	// Resolve private key file path or generated configurations may not work correctly
	var err error
	if args.PrivateKeyFile, err = filepath.Abs(args.PrivateKeyFile); err != nil {
		log.Fatal("Path to Private Key could not be made absolute with error: ", err)
	}

	switch operation := flag.Arg(0); operation {
	case "erase":
		os.Exit(0)
	case "store":
		os.Exit(0)
	case "get":
		if args.InstallationId == 0 && len(args.Organization) == 0 {
			log.Fatal("installationId or Organization is mandatory for get operation")
		}
		doGet(os.Stdout, &args)
	case "generate":
		doGenerate(os.Stdout, &args)
	default:
		printUsage()
		os.Exit(1)
	}
}
