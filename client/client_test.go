package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ctd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/continuity/fs/fstest"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	intoto "github.com/in-toto/in-toto-golang/in_toto"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/exporter"
	"github.com/moby/buildkit/session/exporter/exporterprovider"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/sourcepolicy"
	sourcepolicypb "github.com/moby/buildkit/sourcepolicy/pb"
	"github.com/moby/buildkit/util/attestation"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/moby/buildkit/util/testutil"
	containerdutil "github.com/moby/buildkit/util/testutil/containerd"
	"github.com/moby/buildkit/util/testutil/echoserver"
	"github.com/moby/buildkit/util/testutil/helpers"
	"github.com/moby/buildkit/util/testutil/httpserver"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/moby/buildkit/util/testutil/workers"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/spdx/tools-golang/spdx"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
	fsutiltypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

func init() {
	if workers.IsTestDockerd() {
		workers.InitDockerdWorker()
	} else {
		workers.InitOCIWorker()
		workers.InitContainerdWorker()
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

var allTests = []func(t *testing.T, sb integration.Sandbox){
	testCacheExportCacheKeyLoop,
	testRelativeWorkDir,
	testFileOpMkdirMkfile,
	testFileOpCopyRm,
	testFileOpCopyIncludeExclude,
	testFileOpCopyAlwaysReplaceExistingDestPaths,
	testFileOpRmWildcard,
	testFileOpCopyUIDCache,
	testFileOpCopyChmodText,
	testCallDiskUsage,
	testBuildMultiMount,
	testBuildHTTPSource,
	testBuildHTTPSourceEtagScope,
	testBuildHTTPSourceAuthHeaderSecret,
	testBuildHTTPSourceHostTokenSecret,
	testBuildHTTPSourceHeader,
	testBuildPushAndValidate,
	testBuildExportWithUncompressed,
	testBuildExportScratch,
	testResolveAndHosts,
	testUser,
	testOCIExporter,
	testOCIExporterContentStore,
	testSessionExporter,
	testWhiteoutParentDir,
	testFrontendImageNaming,
	testDuplicateWhiteouts,
	testMountWithNoSource,
	testInvalidExporter,
	testReadonlyRootFS,
	testBasicRegistryCacheImportExport,
	testBasicLocalCacheImportExport,
	testBasicS3CacheImportExport,
	testBasicAzblobCacheImportExport,
	testCachedMounts,
	testCopyFromEmptyImage,
	testProxyEnv,
	testLocalSymlinkEscape,
	testTmpfsMounts,
	testSharedCacheMounts,
	testSharedCacheMountsNoScratch,
	testLockedCacheMounts,
	testDuplicateCacheMount,
	testRunCacheWithMounts,
	testParallelLocalBuilds,
	testSecretEnv,
	testSecretMounts,
	testExtraHosts,
	testShmSize,
	testUlimit,
	testCgroupParent,
	testNetworkMode,
	testFrontendMetadataReturn,
	testFrontendUseSolveResults,
	testSSHMount,
	testRawSocketMount,
	testStdinClosed,
	testHostnameLookup,
	testHostnameSpecifying,
	testPushByDigest,
	testBasicInlineCacheImportExport,
	testExportBusyboxLocal,
	testBridgeNetworking,
	testCacheMountNoCache,
	testExporterTargetExists,
	testTarExporterWithSocket,
	testTarExporterWithSocketCopy,
	testTarExporterSymlink,
	testMultipleRegistryCacheImportExport,
	testMultipleExporters,
	testSourceMap,
	testSourceMapFromRef,
	testLazyImagePush,
	testStargzLazyPull,
	testStargzLazyInlineCacheImportExport,
	testFileOpInputSwap,
	testRelativeMountpoint,
	testLocalSourceDiffer,
	testLocalSourceWithHardlinksFilter,
	testNoTarOCIIndexMediaType,
	testOCILayoutSource,
	testOCILayoutPlatformSource,
	testBuildExportZstd,
	testPullZstdImage,
	testMergeOp,
	testMergeOpCacheInline,
	testMergeOpCacheMin,
	testMergeOpCacheMax,
	testRmSymlink,
	testMoveParentDir,
	testBuildExportWithForeignLayer,
	testZstdLocalCacheExport,
	testCacheExportIgnoreError,
	testZstdRegistryCacheImportExport,
	testZstdLocalCacheImportExport,
	testUncompressedLocalCacheImportExport,
	testUncompressedRegistryCacheImportExport,
	testStargzLazyRegistryCacheImportExport,
	testValidateDigestOrigin,
	testCallInfo,
	testPullWithLayerLimit,
	testExportAnnotations,
	testExportAnnotationsMediaTypes,
	testExportAttestationsOCIArtifact,
	testExportAttestationsImageManifest,
	testExportedImageLabels,
	testAttestationDefaultSubject,
	testSourceDateEpochLayerTimestamps,
	testSourceDateEpochClamp,
	testSourceDateEpochReset,
	testSourceDateEpochLocalExporter,
	testSourceDateEpochTarExporter,
	testSourceDateEpochImageExporter,
	testAttestationBundle,
	testSBOMScan,
	testSBOMScanSingleRef,
	testSBOMSupplements,
	testMultipleCacheExports,
	testMountStubsDirectory,
	testMountStubsTimestamp,
	testSourcePolicy,
	testImageManifestRegistryCacheImportExport,
	testLLBMountPerformance,
	testClientCustomGRPCOpts,
	testMultipleRecordsWithSameLayersCacheImportExport,
	testRegistryEmptyCacheExport,
	testSnapshotWithMultipleBlobs,
	testExportLocalNoPlatformSplit,
	testExportLocalNoPlatformSplitOverwrite,
	testExportLocalForcePlatformSplit,
	testSolverOptLocalDirsStillWorks,
	testOCIIndexMediatype,
	testLayerLimitOnMounts,
	testFrontendVerifyPlatforms,
	testFrontendLintSkipVerifyPlatforms,
	testRunValidExitCodes,
	testFileOpSymlink,
	testMetadataOnlyLocal,
}

func TestIntegration(t *testing.T) {
	testIntegration(t, append(allTests, validationTests...)...)
}

func testIntegration(t *testing.T, funcs ...func(t *testing.T, sb integration.Sandbox)) {
	mirroredImagesUnix := integration.OfficialImages("busybox:latest", "alpine:latest")
	mirroredImagesUnix["tonistiigi/test:nolayers"] = "docker.io/tonistiigi/test:nolayers"
	mirroredImagesUnix["cpuguy83/buildkit-foreign:latest"] = "docker.io/cpuguy83/buildkit-foreign:latest"
	mirroredImagesWin := integration.OfficialImages("nanoserver:latest", "nanoserver:plus")

	mirroredImages := integration.UnixOrWindows(mirroredImagesUnix, mirroredImagesWin)
	mirrors := integration.WithMirroredImages(mirroredImages)

	tests := integration.TestFuncs(funcs...)
	tests = append(tests, diffOpTestCases()...)
	integration.Run(t, tests, mirrors)

	// the rest of the tests are meant for non-Windows, skipping on Windows.
	integration.SkipOnPlatform(t, "windows")

	integration.Run(t, integration.TestFuncs(
		testSecurityMode,
		testSecurityModeSysfs,
		testSecurityModeErrors,
	),
		mirrors,
		integration.WithMatrix("secmode", map[string]any{
			"sandbox":  securitySandbox,
			"insecure": securityInsecure,
		}),
	)

	integration.Run(t, integration.TestFuncs(
		testHostNetworking,
	),
		mirrors,
		integration.WithMatrix("netmode", map[string]any{
			"default": defaultNetwork,
			"host":    hostNetwork,
		}),
	)

	integration.Run(
		t,
		integration.TestFuncs(testBridgeNetworkingDNSNoRootless),
		mirrors,
		integration.WithMatrix("netmode", map[string]any{
			"dns": bridgeDNSNetwork,
		}),
	)

	integration.Run(t, integration.TestFuncs(cdiTests...), mirrors)
}

func newContainerd(cdAddress string) (*ctd.Client, error) {
	return ctd.New(cdAddress, ctd.WithTimeout(60*time.Second))
}

// moby/buildkit#1336
func testCacheExportCacheKeyLoop(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureCacheExport, workers.FeatureCacheBackendLocal)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	tmpdir := integration.Tmpdir(t)

	err = os.WriteFile(filepath.Join(tmpdir.Name, "foo"), []byte("foodata"), 0600)
	require.NoError(t, err)

	imgName := integration.UnixOrWindows("alpine:latest", "nanoserver:latest")
	scratch := integration.UnixOrWindows(
		llb.Scratch(),
		llb.Image(imgName),
	)
	cmdStr := integration.UnixOrWindows("true", "cmd /C exit 0")
	for _, mode := range []bool{false, true} {
		func(mode bool) {
			t.Run(fmt.Sprintf("mode=%v", mode), func(t *testing.T) {
				buildbase := llb.Image(imgName).File(llb.Copy(llb.Local("mylocal"), "foo", "foo"))
				if mode { // same cache keys with a separating node go to different code-path
					buildbase = buildbase.Run(llb.Shlex(cmdStr)).Root()
				}
				intermed := llb.Image(imgName).File(llb.Copy(buildbase, "foo", "foo"))
				final := scratch.File(llb.Copy(intermed, "foo", "foooooo"))

				def, err := final.Marshal(sb.Context())
				require.NoError(t, err)

				_, err = c.Solve(sb.Context(), def, SolveOpt{
					CacheExports: []CacheOptionsEntry{
						{
							Type: "local",
							Attrs: map[string]string{
								"dest": filepath.Join(tmpdir.Name, "cache"),
							},
						},
					},
					LocalMounts: map[string]fsutil.FS{
						"mylocal": tmpdir,
					},
				}, nil)
				require.NoError(t, err)
			})
		}(mode)
	}
}

func testBridgeNetworking(t *testing.T, sb integration.Sandbox) {
	if os.Getenv("BUILDKIT_RUN_NETWORK_INTEGRATION_TESTS") == "" {
		t.SkipNow()
	}
	if sb.Rootless() { // bridge is not used by default, even with detach-netns
		t.SkipNow()
	}
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	s, err := echoserver.NewTestServer("foo")
	require.NoError(t, err)
	addrParts := strings.Split(s.Addr().String(), ":")

	def, err := llb.Image("busybox").Run(llb.Shlexf("sh -c 'nc 127.0.0.1 %s | grep foo'", addrParts[len(addrParts)-1])).Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
}

func testBridgeNetworkingDNSNoRootless(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureCNINetwork)
	if os.Getenv("BUILDKIT_RUN_NETWORK_INTEGRATION_TESTS") == "" {
		t.SkipNow()
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	name := identity.NewID()
	server, err := llb.Image("busybox").
		Run(
			llb.Shlexf(`sh -c 'test "$(nc -l -p 1234)" = "foo"'`),
			llb.Hostname(name),
		).
		Marshal(sb.Context())
	require.NoError(t, err)

	client, err := llb.Image("busybox").
		Run(
			llb.Shlexf("sh -c 'until echo foo | nc " + name + " 1234 -w0; do sleep 0.1; done'"),
		).
		Marshal(sb.Context())
	require.NoError(t, err)

	eg, ctx := errgroup.WithContext(context.Background())
	eg.Go(func() error {
		_, err := c.Solve(ctx, server, SolveOpt{}, nil)
		return err
	})
	eg.Go(func() error {
		_, err := c.Solve(ctx, client, SolveOpt{}, nil)
		return err
	})
	err = eg.Wait()
	require.NoError(t, err)
}

func testHostNetworking(t *testing.T, sb integration.Sandbox) {
	if os.Getenv("BUILDKIT_RUN_NETWORK_INTEGRATION_TESTS") == "" {
		t.SkipNow()
	}
	netMode := sb.Value("netmode")
	var allowedEntitlements []string
	if netMode == hostNetwork {
		allowedEntitlements = []string{entitlements.EntitlementNetworkHost.String()}
	}
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	s, err := echoserver.NewTestServer("foo")
	require.NoError(t, err)
	addrParts := strings.Split(s.Addr().String(), ":")

	def, err := llb.Image("busybox").Run(llb.Shlexf("sh -c 'nc 127.0.0.1 %s | grep foo'", addrParts[len(addrParts)-1]), llb.Network(llb.NetModeHost)).Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		AllowedEntitlements: allowedEntitlements,
	}, nil)
	if netMode == hostNetwork {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
}

func testExportedImageLabels(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Skip("only supported with containerd")
	}

	ctx := sb.Context()

	imgName := integration.UnixOrWindows("busybox", "nanoserver")
	prefix := integration.UnixOrWindows(
		"",
		"cmd /C ", // TODO(profnandaa): currently needs the shell prefix, to be fixed
	)
	def, err := llb.Image(imgName).
		Run(llb.Shlexf(fmt.Sprintf("%secho foo > /foo", prefix))).
		Marshal(ctx)
	require.NoError(t, err)

	target := "docker.io/buildkit/build/exporter:labels"

	_, err = c.Solve(ctx, def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx = namespaces.WithNamespace(ctx, "buildkit")

	img, err := client.GetImage(ctx, target)
	require.NoError(t, err)

	store := client.ContentStore()

	info, err := store.Info(ctx, img.Target().Digest)
	require.NoError(t, err)

	dt, err := content.ReadBlob(ctx, store, img.Target())
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(dt, &mfst)
	require.NoError(t, err)

	require.Equal(t, 2, len(mfst.Layers))

	hasLabel := func(dgst digest.Digest) bool {
		for k, v := range info.Labels {
			if strings.HasPrefix(k, "containerd.io/gc.ref.content.") && v == dgst.String() {
				return true
			}
		}
		return false
	}

	// check that labels are set on all layers and config
	for _, l := range mfst.Layers {
		require.True(t, hasLabel(l.Digest))
	}
	require.True(t, hasLabel(mfst.Config.Digest))

	err = c.Prune(sb.Context(), nil, PruneAll)
	require.NoError(t, err)

	// layer should not be deleted
	_, err = store.Info(ctx, mfst.Layers[1].Digest)
	require.NoError(t, err)

	err = client.ImageService().Delete(ctx, target, images.SynchronousDelete())
	require.NoError(t, err)

	// layers should be deleted
	_, err = store.Info(ctx, mfst.Layers[1].Digest)
	require.Error(t, err)
	require.True(t, errors.Is(err, cerrdefs.ErrNotFound))

	// config should be deleted
	_, err = store.Info(ctx, mfst.Config.Digest)
	require.Error(t, err)
	require.True(t, errors.Is(err, cerrdefs.ErrNotFound))

	// buildkit contentstore still has the layer because it is multi-ns
	bkstore := proxy.NewContentStore(c.ContentClient())

	// layer should be deleted as not kept by history
	_, err = bkstore.Info(ctx, mfst.Layers[1].Digest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// config should still be there
	_, err = bkstore.Info(ctx, img.Metadata().Target.Digest)
	require.NoError(t, err)

	_, err = bkstore.Info(ctx, mfst.Config.Digest)
	require.NoError(t, err)

	cl, err := c.ControlClient().ListenBuildHistory(sb.Context(), &controlapi.BuildHistoryRequest{
		EarlyExit: true,
	})
	require.NoError(t, err)

	for {
		resp, err := cl.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		_, err = c.ControlClient().UpdateBuildHistory(sb.Context(), &controlapi.UpdateBuildHistoryRequest{
			Ref:    resp.Record.Ref,
			Delete: true,
		})
		require.NoError(t, err)
	}

	// now everything should be deleted
	_, err = bkstore.Info(ctx, img.Metadata().Target.Digest)
	require.Error(t, err)

	_, err = bkstore.Info(ctx, mfst.Config.Digest)
	require.Error(t, err)
}

// #877
func testExportBusyboxLocal(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	def, err := llb.Image("busybox").Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	fi, err := os.Stat(filepath.Join(destDir, "bin/busybox"))
	require.NoError(t, err)

	fi2, err := os.Stat(filepath.Join(destDir, "bin/vi"))
	require.NoError(t, err)

	require.True(t, os.SameFile(fi, fi2))
}

func testHostnameLookup(t *testing.T, sb integration.Sandbox) {
	if sb.Rootless() { // bridge is not used by default, even with detach-netns
		t.SkipNow()
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	cmdStr := integration.UnixOrWindows(
		`sh -c "ping -c 1 $(hostname)"`,
		"cmd /C ping -n 1 %COMPUTERNAME%",
	)
	st := llb.Image(imgName).Run(llb.Shlex(cmdStr))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

// moby/buildkit#1301
func testHostnameSpecifying(t *testing.T, sb integration.Sandbox) {
	if sb.Rootless() { // bridge is not used by default, even with detach-netns
		t.SkipNow()
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	hostname := "testtest"
	// NOTE: Windows capitalizes the hostname hence
	// case insensitive findstr (findstr /I)
	// testtest --> TESTTEST
	cmdStr1 := integration.UnixOrWindows(
		"sh -c 'echo $HOSTNAME | grep %s'",
		"cmd /C echo %%COMPUTERNAME%% | findstr /I %s",
	)
	cmdStr2 := integration.UnixOrWindows(
		"sh -c 'echo $(hostname) | grep %s'",
		"cmd /C echo %%COMPUTERNAME%% | findstr /I %s",
	)
	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	st := llb.Image(imgName).With(llb.Hostname(hostname)).
		Run(llb.Shlexf(cmdStr1, hostname)).
		Run(llb.Shlexf(cmdStr2, hostname))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{"hostname": hostname},
	}, nil)
	require.NoError(t, err)
}

// moby/buildkit#614
func testStdinClosed(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	cmdStr := integration.UnixOrWindows("cat", "cmd /C more")
	st := llb.Image(imgName).Run(llb.Shlex(cmdStr))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testSSHMount(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	a := agent.NewKeyring()

	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	err = a.Add(agent.AddedKey{PrivateKey: k})
	require.NoError(t, err)

	sockPath, err := makeSSHAgentSock(t, a)
	require.NoError(t, err)

	ssh, err := sshprovider.NewSSHAgentProvider([]sshprovider.AgentConfig{{
		Paths: []string{sockPath},
	}})
	require.NoError(t, err)

	// no ssh exposed
	st := llb.Image("busybox:latest").Run(llb.Shlex(`nosuchcmd`), llb.AddSSHSocket())
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no SSH key ")

	// custom ID not exposed
	st = llb.Image("busybox:latest").Run(llb.Shlex(`nosuchcmd`), llb.AddSSHSocket(llb.SSHID("customID")))
	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{ssh},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unset ssh forward key customID")

	// missing custom ID ignored on optional
	st = llb.Image("busybox:latest").Run(llb.Shlex(`ls`), llb.AddSSHSocket(llb.SSHID("customID"), llb.SSHOptional))
	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{ssh},
	}, nil)
	require.NoError(t, err)

	// valid socket
	st = llb.Image("alpine:latest").
		Run(llb.Shlex(`apk add --no-cache openssh`)).
		Run(llb.Shlex(`sh -c 'echo -n $SSH_AUTH_SOCK > /out/sock && ssh-add -l > /out/out'`),
			llb.AddSSHSocket())

	out := st.AddMount("/out", llb.Scratch())
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		Session: []session.Attachable{ssh},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "sock"))
	require.NoError(t, err)
	require.Equal(t, "/run/buildkit/ssh_agent.0", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "out"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "2048")
	require.Contains(t, string(dt), "(RSA)")

	// forbidden command
	st = llb.Image("alpine:latest").
		Run(llb.Shlex(`apk add --no-cache openssh`)).
		Run(llb.Shlex(`sh -c 'ssh-keygen -f /tmp/key -N "" && ssh-add -k /tmp/key 2> /out/out || true'`),
			llb.AddSSHSocket())

	out = st.AddMount("/out", llb.Scratch())
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		Session: []session.Attachable{ssh},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(filepath.Join(destDir, "out"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "agent refused operation")

	// valid socket from key on disk
	st = llb.Image("alpine:latest").
		Run(llb.Shlex(`apk add --no-cache openssh`)).
		Run(llb.Shlex(`sh -c 'ssh-add -l > /out/out'`),
			llb.AddSSHSocket())

	out = st.AddMount("/out", llb.Scratch())
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	k, err = rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	dt = pem.EncodeToMemory(
		&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(k),
		},
	)

	tmpDir := t.TempDir()

	err = os.WriteFile(filepath.Join(tmpDir, "key"), dt, 0600)
	require.NoError(t, err)

	ssh, err = sshprovider.NewSSHAgentProvider([]sshprovider.AgentConfig{{
		Paths: []string{filepath.Join(tmpDir, "key")},
	}})
	require.NoError(t, err)

	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		Session: []session.Attachable{ssh},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(filepath.Join(destDir, "out"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "2048")
	require.Contains(t, string(dt), "(RSA)")
}

func testRawSocketMount(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	l, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer l.Close()

	var called atomic.Bool
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(l)
	defer srv.Close()

	st := llb.Image("alpine:latest").
		Run(llb.Shlex(`apk add --no-cache curl`)).
		Run(llb.Shlex(`curl --unix-socket /tmp/test.sock http://./foo`),
			llb.AddSSHSocket(llb.SSHSocketTarget("/tmp/test.sock")),
		).Root()

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	raw, err := sshprovider.NewSSHAgentProvider([]sshprovider.AgentConfig{{
		Paths: []string{sockPath},
		Raw:   true,
	}})
	require.NoError(t, err)

	ch := make(chan *SolveStatus)
	go func() {
		for status := range ch {
			for _, l := range status.Logs {
				t.Log(string(l.Data))
			}
		}
	}()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{raw},
	}, ch)
	require.NoError(t, err)
	require.True(t, called.Load(), "server should have been called")
}

func testExtraHosts(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c 'cat /etc/hosts | grep myhost | grep 1.2.3.4'`), llb.AddExtraHost("myhost", net.ParseIP("1.2.3.4")))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testShmSize(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").Run(
		llb.AddMount("/dev/shm", llb.Scratch(), llb.Tmpfs(llb.TmpfsSize(128*1024*1024))),
		llb.Shlex(`sh -c 'mount | grep /dev/shm > /out/out'`),
	)

	out := st.AddMount("/out", llb.Scratch())
	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "out"))
	require.NoError(t, err)
	require.Contains(t, string(dt), `size=131072k`)
}

func testUlimit(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string, ro ...llb.RunOption) {
		st = busybox.Run(append(ro, llb.Shlex(cmd), llb.Dir("/wd"))...).AddMount("/wd", st)
	}

	run(`sh -c "ulimit -n > first"`, llb.AddUlimit(llb.UlimitNofile, 1062, 1062))
	run(`sh -c "ulimit -n > second"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "first"))
	require.NoError(t, err)
	require.Equal(t, `1062`, strings.TrimSpace(string(dt)))

	dt2, err := os.ReadFile(filepath.Join(destDir, "second"))
	require.NoError(t, err)
	require.NotEqual(t, `1062`, strings.TrimSpace(string(dt2)))
}

func testCgroupParent(t *testing.T, sb integration.Sandbox) {
	if sb.Rootless() {
		t.SkipNow()
	}

	if _, err := os.Lstat("/sys/fs/cgroup/cgroup.subtree_control"); os.IsNotExist(err) {
		t.Skipf("test requires cgroup v2")
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	img := llb.Image("alpine:latest")
	st := llb.Scratch()

	run := func(cmd string, ro ...llb.RunOption) {
		st = img.Run(append(ro, llb.Shlex(cmd), llb.Dir("/wd"))...).AddMount("/wd", st)
	}

	cgroupName := "test." + identity.NewID()

	err = os.MkdirAll(filepath.Join("/sys/fs/cgroup", cgroupName), 0755)
	require.NoError(t, err)

	defer func() {
		err := os.RemoveAll(filepath.Join("/sys/fs/cgroup", cgroupName))
		require.NoError(t, err)
	}()

	err = os.WriteFile(filepath.Join("/sys/fs/cgroup", cgroupName, "pids.max"), []byte("10"), 0644)
	require.NoError(t, err)

	run(`sh -c "(for i in $(seq 1 10); do sleep 1 & done 2>first.error); cat /proc/self/cgroup >> first"`, llb.WithCgroupParent(cgroupName))
	run(`sh -c "(for i in $(seq 1 10); do sleep 1 & done 2>second.error); cat /proc/self/cgroup >> second"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	// neither process leaks parent cgroup name inside container
	dt, err := os.ReadFile(filepath.Join(destDir, "first"))
	require.NoError(t, err)
	require.NotContains(t, strings.TrimSpace(string(dt)), cgroupName)

	dt2, err := os.ReadFile(filepath.Join(destDir, "second"))
	require.NoError(t, err)
	require.NotContains(t, strings.TrimSpace(string(dt2)), cgroupName)

	dt, err = os.ReadFile(filepath.Join(destDir, "first.error"))
	require.NoError(t, err)
	require.Contains(t, strings.TrimSpace(string(dt)), "Resource temporarily unavailable")

	dt, err = os.ReadFile(filepath.Join(destDir, "second.error"))
	require.NoError(t, err)
	require.Equal(t, "", strings.TrimSpace(string(dt)))
}

func testNetworkMode(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c 'wget https://example.com 2>&1 | grep "wget: bad address"'`), llb.Network(llb.NetModeNone))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	st2 := llb.Image("busybox:latest").
		Run(llb.Shlex(`ifconfig`), llb.Network(llb.NetModeHost))

	def, err = st2.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		// Currently disabled globally by default
		// AllowedEntitlements: []entitlements.Entitlement{entitlements.EntitlementNetworkHost},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "network.host is not allowed")
}

func testPushByDigest(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	st := llb.Scratch().File(llb.Mkfile("foo", 0600, []byte("data")))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	name := registry + "/foo/bar"

	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: "image",
				Attrs: map[string]string{
					"name":           name,
					"push":           "true",
					"push-by-digest": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	_, _, err = contentutil.ProviderFromRef(name + ":latest")
	require.Error(t, err)

	desc, _, err := contentutil.ProviderFromRef(name + "@" + resp.ExporterResponse[exptypes.ExporterImageDigestKey])
	require.NoError(t, err)

	require.Equal(t, resp.ExporterResponse[exptypes.ExporterImageDigestKey], desc.Digest.String())
	require.Equal(t, images.MediaTypeDockerSchema2Manifest, desc.MediaType)
	require.Greater(t, desc.Size, int64(0))
}

func testSecurityMode(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureSecurityMode)
	command := `sh -c 'cat /proc/self/status | grep CapEff | cut -f 2 > /out'`
	mode := llb.SecurityModeSandbox
	var allowedEntitlements []string
	var assertCaps func(caps uint64)
	secMode := sb.Value("secmode")
	if secMode == securitySandbox {
		assertCaps = func(caps uint64) {
			/*
				$ capsh --decode=00000000a80425fb
				0x00000000a80425fb=cap_chown,cap_dac_override,cap_fowner,cap_fsetid,cap_kill,cap_setgid,cap_setuid,cap_setpcap,
				cap_net_bind_service,cap_net_raw,cap_sys_chroot,cap_mknod,cap_audit_write,cap_setfcap
			*/
			require.Equal(t, uint64(0xa80425fb), caps)
		}
		allowedEntitlements = []string{}
	} else {
		assertCaps = func(caps uint64) {
			/*
				$ capsh --decode=0000003fffffffff
				0x0000003fffffffff=cap_chown,cap_dac_override,cap_dac_read_search,cap_fowner,cap_fsetid,cap_kill,cap_setgid,
				cap_setuid,cap_setpcap,cap_linux_immutable,cap_net_bind_service,cap_net_broadcast,cap_net_admin,cap_net_raw,
				cap_ipc_lock,cap_ipc_owner,cap_sys_module,cap_sys_rawio,cap_sys_chroot,cap_sys_ptrace,cap_sys_pacct,cap_sys_admin,
				cap_sys_boot,cap_sys_nice,cap_sys_resource,cap_sys_time,cap_sys_tty_config,cap_mknod,cap_lease,cap_audit_write,
				cap_audit_control,cap_setfcap,cap_mac_override,cap_mac_admin,cap_syslog,cap_wake_alarm,cap_block_suspend,cap_audit_read
			*/

			// require that _at least_ minimum capabilities are granted
			require.Equal(t, uint64(0x3fffffffff), caps&0x3fffffffff)
		}
		mode = llb.SecurityModeInsecure
		allowedEntitlements = []string{entitlements.EntitlementSecurityInsecure.String()}
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(command),
			llb.Security(mode))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		AllowedEntitlements: allowedEntitlements,
	}, nil)

	require.NoError(t, err)

	contents, err := os.ReadFile(filepath.Join(destDir, "out"))
	require.NoError(t, err)

	caps, err := strconv.ParseUint(strings.TrimSpace(string(contents)), 16, 64)
	require.NoError(t, err)

	t.Logf("Caps: %x", caps)

	assertCaps(caps)
}

func testSecurityModeSysfs(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureSecurityMode)
	if sb.Rootless() {
		t.SkipNow()
	}

	mode := llb.SecurityModeSandbox
	var allowedEntitlements []string
	secMode := sb.Value("secmode")
	if secMode == securitySandbox {
		allowedEntitlements = []string{}
	} else {
		mode = llb.SecurityModeInsecure
		allowedEntitlements = []string{entitlements.EntitlementSecurityInsecure.String()}
	}

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	cg := "/sys/fs/cgroup/cpuset/securitytest" // cgroup v1
	if _, err := os.Stat("/sys/fs/cgroup/cpuset"); errors.Is(err, os.ErrNotExist) {
		cg = "/sys/fs/cgroup/securitytest" // cgroup v2
	}

	command := "mkdir " + cg
	st := llb.Image("busybox:latest").
		Run(llb.Shlex(command),
			llb.Security(mode))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		AllowedEntitlements: allowedEntitlements,
	}, nil)

	if secMode == securitySandbox {
		require.Error(t, err)
		require.Contains(t, err.Error(), "did not complete successfully")
		require.Contains(t, err.Error(), "mkdir "+cg)
	} else {
		require.NoError(t, err)
	}
}

func testSecurityModeErrors(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()
	secMode := sb.Value("secmode")
	if secMode == securitySandbox {
		st := llb.Image("busybox:latest").
			Run(llb.Shlex(`sh -c 'echo sandbox'`))

		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)

		_, err = c.Solve(sb.Context(), def, SolveOpt{
			AllowedEntitlements: []string{entitlements.EntitlementSecurityInsecure.String()},
		}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "security.insecure is not allowed")
	}
	if secMode == securityInsecure {
		st := llb.Image("busybox:latest").
			Run(llb.Shlex(`sh -c 'echo insecure'`), llb.Security(llb.SecurityModeInsecure))

		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)

		_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "security.insecure is not allowed")
	}
}

func testFrontendImageNaming(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	checkImageName := map[string]func(out, imageName string, exporterResponse map[string]string){
		ExporterOCI: func(out, imageName string, exporterResponse map[string]string) {
			// Nothing to check
		},
		ExporterDocker: func(out, imageName string, exporterResponse map[string]string) {
			require.Contains(t, exporterResponse, exptypes.ExporterImageNameKey)
			require.Equal(t, exporterResponse[exptypes.ExporterImageNameKey], "docker.io/library/"+imageName)

			dt, err := os.ReadFile(out)
			require.NoError(t, err)

			m, err := testutil.ReadTarToMap(dt, false)
			require.NoError(t, err)

			_, ok := m["oci-layout"]
			require.True(t, ok)

			var index ocispecs.Index
			err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
			require.NoError(t, err)
			require.Equal(t, 2, index.SchemaVersion)
			require.Equal(t, 1, len(index.Manifests))

			var dockerMfst []struct {
				RepoTags []string
			}
			err = json.Unmarshal(m["manifest.json"].Data, &dockerMfst)
			require.NoError(t, err)
			require.Equal(t, 1, len(dockerMfst))
			require.Equal(t, 1, len(dockerMfst[0].RepoTags))
			require.Equal(t, imageName, dockerMfst[0].RepoTags[0])
		},
		ExporterImage: func(_, imageName string, exporterResponse map[string]string) {
			require.Contains(t, exporterResponse, exptypes.ExporterImageNameKey)
			require.Equal(t, exporterResponse[exptypes.ExporterImageNameKey], imageName)

			// check if we can pull (requires containerd)
			cdAddress := sb.ContainerdAddress()
			if cdAddress == "" {
				return
			}

			// TODO: make public pull helper function so this can be checked for standalone as well

			client, err := ctd.New(cdAddress)
			require.NoError(t, err)
			defer client.Close()

			ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

			// check image in containerd
			_, err = client.ImageService().Get(ctx, imageName)
			require.NoError(t, err)

			// deleting image should release all content
			err = client.ImageService().Delete(ctx, imageName, images.SynchronousDelete())
			require.NoError(t, err)

			checkAllReleasable(t, c, sb, true)

			_, err = client.Pull(ctx, imageName)
			require.NoError(t, err)

			err = client.ImageService().Delete(ctx, imageName, images.SynchronousDelete())
			require.NoError(t, err)
		},
	}

	// A caller provided name takes precedence over one returned by the frontend. Iterate over both options.
	for _, winner := range []string{"frontend", "caller"} {
		// The double layer of `t.Run` here is required so
		// that the inner-most tests (with the actual
		// functionality) have definitely completed before the
		// sandbox and registry cleanups (defered above) are run.
		t.Run(winner, func(t *testing.T) {
			for _, exp := range []string{ExporterOCI, ExporterDocker, ExporterImage} {
				t.Run(exp, func(t *testing.T) {
					destDir := t.TempDir()

					so := SolveOpt{
						Exports: []ExportEntry{
							{
								Type:  exp,
								Attrs: map[string]string{},
							},
						},
					}

					out := filepath.Join(destDir, "out.tar")

					imageName := "image-" + exp + "-fe:latest"

					switch exp {
					case ExporterOCI:
						workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
						t.Skip("oci exporter does not support named images")
					case ExporterDocker:
						workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
						outW, err := os.Create(out)
						require.NoError(t, err)
						so.Exports[0].Output = fixedWriteCloser(outW)
					case ExporterImage:
						workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
						imageName = registry + "/" + imageName
						so.Exports[0].Attrs["push"] = "true"
					}

					feName := imageName
					switch winner {
					case "caller":
						feName = "loser:latest"
						so.Exports[0].Attrs["name"] = imageName
					case "frontend":
						so.Exports[0].Attrs["name"] = "*"
					}

					frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
						res := gateway.NewResult()
						res.AddMeta("image.name", []byte(feName))
						return res, nil
					}

					resp, err := c.Build(sb.Context(), so, "", frontend, nil)
					require.NoError(t, err)

					checkImageName[exp](out, imageName, resp.ExporterResponse)
				})
			}
		})
	}

	checkAllReleasable(t, c, sb, true)
}

func testSecretMounts(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c 'mount | grep mysecret | grep "type tmpfs" && [ "$(cat /run/secrets/mysecret)" = 'foo-secret' ]'`), llb.AddSecret("/run/secrets/mysecret"))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
			"/run/secrets/mysecret": []byte("foo-secret"),
		})},
	}, nil)
	require.NoError(t, err)

	// test optional, mount should not exist when secret not present in SolveOpt
	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`test ! -f /run/secrets/mysecret2`), llb.AddSecret("/run/secrets/mysecret2", llb.SecretOptional))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{})},
	}, nil)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`echo secret3`), llb.AddSecret("/run/secrets/mysecret3"))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{})},
	}, nil)
	require.Error(t, err)

	// test id,perm,uid
	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c '[ "$(stat -c "%u %g %f" /run/secrets/mysecret4)" = "1 1 81ff" ]' `), llb.AddSecret("/run/secrets/mysecret4", llb.SecretID("mysecret"), llb.SecretFileOpt(1, 1, 0777)))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
			"mysecret": []byte("pw"),
		})},
	}, nil)
	require.NoError(t, err)

	// test empty cert still creates secret file
	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`test -f /run/secrets/mysecret5`), llb.AddSecret("/run/secrets/mysecret5", llb.SecretID("mysecret")))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
			"mysecret": []byte(""),
		})},
	}, nil)
	require.NoError(t, err)
}

func testSecretEnv(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c '[ "$(echo ${MY_SECRET})" = 'foo-secret' ]'`), llb.AddSecret("MY_SECRET", llb.SecretAsEnv(true)))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
			"MY_SECRET": []byte("foo-secret"),
		})},
	}, nil)
	require.NoError(t, err)

	// test optional
	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c '[ -z "${MY_SECRET}" ]'`), llb.AddSecret("MY_SECRET", llb.SecretAsEnv(true), llb.SecretOptional))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{})},
	}, nil)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`echo foo`), llb.AddSecret("MY_SECRET", llb.SecretAsEnv(true)))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{})},
	}, nil)
	require.Error(t, err)

	// test id
	st = llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c '[ "$(echo ${MYPASSWORD}-${MYTOKEN})" = "pw-token" ]' `),
			llb.AddSecret("MYPASSWORD", llb.SecretID("pass"), llb.SecretAsEnv(true)),
			llb.AddSecret("MYTOKEN", llb.SecretAsEnv(true)),
		)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
			"pass":    []byte("pw"),
			"MYTOKEN": []byte("token"),
		})},
	}, nil)
	require.NoError(t, err)
}

func testTmpfsMounts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh -c 'mount | grep /foobar | grep "type tmpfs" && touch /foobar/test'`), llb.AddMount("/foobar", llb.Scratch(), llb.Tmpfs()))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testLocalSymlinkEscape(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	test := []byte(`set -ex
[[ -L /mount/foo ]]
[[ -L /mount/sub/bar ]]
[[ -L /mount/bax ]]
[[ -f /mount/bay ]]
[[ -f /mount/sub/sub2/file ]]
[[ ! -f /mount/baz ]]
[[ ! -f /mount/etc/passwd ]]
[[ ! -f /mount/etc/group ]]
[[ $(readlink /mount/foo) == "/etc/passwd" ]]
[[ $(readlink /mount/sub/bar) == "../../../etc/group" ]]
`)

	dir := integration.Tmpdir(
		t,
		// point to absolute path that is not part of dir
		fstest.Symlink("/etc/passwd", "foo"),
		fstest.CreateDir("sub", 0700),
		// point outside of the dir
		fstest.Symlink("../../../etc/group", "sub/bar"),
		// regular valid symlink
		fstest.Symlink("bay", "bax"),
		// target for symlink (not requested)
		fstest.CreateFile("bay", []byte{}, 0600),
		// file with many subdirs
		fstest.CreateDir("sub/sub2", 0700),
		fstest.CreateFile("sub/sub2/file", []byte{}, 0600),
		// unused file that shouldn't be included
		fstest.CreateFile("baz", []byte{}, 0600),
		fstest.CreateFile("test.sh", test, 0700),
	)

	local := llb.Local("mylocal", llb.FollowPaths([]string{
		"test.sh", "foo", "sub/bar", "bax", "sub/sub2/file",
	}))

	st := llb.Image("busybox:latest").
		Run(llb.Shlex(`sh /mount/test.sh`), llb.AddMount("/mount", local, llb.Readonly))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			"mylocal": dir,
		},
	}, nil)
	require.NoError(t, err)
}

func testRelativeWorkDir(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows(
		"docker.io/library/busybox:latest",
		"mcr.microsoft.com/windows/nanoserver:ltsc2022",
	)
	cmdStr := integration.UnixOrWindows(
		`sh -c "pwd > /out/pwd"`,
		`cmd /C "cd > /out/pwd"`,
	)
	pwd := llb.Image(imgName).
		Dir("test1").
		Dir("test2").
		Run(llb.Shlex(cmdStr)).
		AddMount("/out", llb.Scratch())

	def, err := pwd.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "pwd"))
	require.NoError(t, err)
	pathStr := integration.UnixOrWindows(
		"/test1/test2\n",
		"C:\\test1\\test2\r\n",
	)
	require.Equal(t, []byte(pathStr), dt)
}

// TODO: remove this test once `client.SolveOpt.LocalDirs`, now marked as deprecated, is removed.
// For more context on this test, please check:
// https://github.com/moby/buildkit/pull/4583#pullrequestreview-1847043452
func testSolverOptLocalDirsStillWorks(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows(
		"docker.io/library/busybox:latest",
		"mcr.microsoft.com/windows/nanoserver:ltsc2022",
	)
	cmdStr := integration.UnixOrWindows(
		`sh -c "/bin/rev < input.txt > /out/output.txt"`,
		`cmd /C "type input.txt > /out/output.txt"`,
	)
	out := llb.Image(imgName).
		File(llb.Copy(llb.Local("mylocal"), "input.txt", "input.txt")).
		Run(llb.Shlex(cmdStr)).
		AddMount(`/out`, llb.Scratch())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	srcDir := integration.Tmpdir(t,
		fstest.CreateFile("input.txt", []byte("Hello World"), 0600),
	)

	destDir := integration.Tmpdir(t)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		LocalDirs: map[string]string{
			"mylocal": srcDir.Name,
		},
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir.Name,
			},
		},
	}, nil)

	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir.Name, "output.txt"))
	require.NoError(t, err)
	// not reversed on Windows since there's no handy rev utility
	revStr := integration.UnixOrWindows(
		"dlroW olleH",
		"Hello World",
	)
	require.Equal(t, []byte(revStr), dt)
}

func testFileOpMkdirMkfile(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Scratch().
		File(llb.Mkdir("/foo", 0700).Mkfile("bar", 0600, []byte("contents")))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	fi, err := os.Stat(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, true, fi.IsDir())

	dt, err := os.ReadFile(filepath.Join(destDir, "bar"))
	require.NoError(t, err)
	require.Equal(t, []byte("contents"), dt)
}

func testFileOpCopyRm(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("myfile", []byte("data0"), 0600),
		fstest.CreateDir("sub", 0700),
		fstest.CreateFile("sub/foo", []byte("foo0"), 0600),
		fstest.CreateFile("sub/bar", []byte("bar0"), 0600),
	)
	dir2 := integration.Tmpdir(
		t,
		fstest.CreateFile("file2", []byte("file2"), 0600),
	)

	st := llb.Scratch().
		File(
			llb.Copy(llb.Local("mylocal"), "myfile", "myfile2").
				Copy(llb.Local("mylocal"), "sub", "out").
				Rm("out/foo").
				Copy(llb.Local("mylocal2"), "file2", "/"))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal":  dir,
			"mylocal2": dir2,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "myfile2"))
	require.NoError(t, err)
	require.Equal(t, []byte("data0"), dt)

	fi, err := os.Stat(filepath.Join(destDir, "out"))
	require.NoError(t, err)
	require.Equal(t, true, fi.IsDir())

	dt, err = os.ReadFile(filepath.Join(destDir, "out/bar"))
	require.NoError(t, err)
	require.Equal(t, []byte("bar0"), dt)

	_, err = os.Stat(filepath.Join(destDir, "out/foo"))
	require.ErrorIs(t, err, os.ErrNotExist)

	dt, err = os.ReadFile(filepath.Join(destDir, "file2"))
	require.NoError(t, err)
	require.Equal(t, []byte("file2"), dt)
}

func testFileOpCopyChmodText(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	tcases := []struct {
		src  string
		dest string
		mode string
	}{
		{"file", "f1", "go-w"},
		{"file", "f2", "o-rwx,g+x"},
		{"file", "f3", "u=rwx,g=,o="},
		{"file", "f4", "u+rw,g+r,o-x,o+w"},
		{"dir", "d1", "a+X"},
		{"dir", "d2", "g+rw,o+rw"},
		{"dir", "d3", "u=rwX,go=rX"},
	}

	st := llb.Image("alpine").
		Run(llb.Shlex(`sh -c "mkdir /input && touch /input/file && mkdir /input/dir && chmod 0500 /input/dir && mkdir /expected"`))

	for _, tc := range tcases {
		st = st.Run(llb.Shlex(`sh -c "cp -a /input/` + tc.src + ` /expected/` + tc.dest + ` && chmod ` + tc.mode + ` /expected/` + tc.dest + `"`))
	}
	cp := llb.Scratch()

	for _, tc := range tcases {
		cp = cp.File(llb.Copy(st.Root(), "/input/"+tc.src, "/"+tc.dest, llb.ChmodOpt{ModeStr: tc.mode}))
	}

	for _, tc := range tcases {
		st = st.Run(llb.Shlex(`sh -c '[ "$(stat -c '%A' /expected/`+tc.dest+`)" == "$(stat -c '%A' /actual/`+tc.dest+`)" ]'`), llb.AddMount("/actual", cp, llb.Readonly))
	}

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

// moby/buildkit#3291
func testFileOpCopyUIDCache(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Scratch().File(
		llb.Copy(llb.Image("alpine").Run(llb.Shlex(`sh -c 'echo 123 > /foo && chown 1000:1000 /foo'`)).Root(), "foo", "foo"))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(&nopWriteCloser{&buf}),
			},
		},
	}, nil)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(buf.Bytes(), false)
	require.NoError(t, err)

	fi, ok := m["foo"]
	require.True(t, ok)
	require.Equal(t, 1000, fi.Header.Uid)
	require.Equal(t, 1000, fi.Header.Gid)

	// repeat to check cache does not apply for different uid
	st = llb.Scratch().File(
		llb.Copy(llb.Image("alpine").Run(llb.Shlex(`sh -c 'echo 123 > /foo'`)).Root(), "foo", "foo"))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	buf = bytes.Buffer{}
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(&nopWriteCloser{&buf}),
			},
		},
	}, nil)
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(buf.Bytes(), false)
	require.NoError(t, err)

	fi, ok = m["foo"]
	require.True(t, ok)
	require.Equal(t, 0, fi.Header.Uid)
	require.Equal(t, 0, fi.Header.Gid)
}

func testFileOpCopyIncludeExclude(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("myfile", []byte("data0"), 0600),
		fstest.CreateDir("sub", 0700),
		fstest.CreateFile("sub/foo", []byte("foo0"), 0600),
		fstest.CreateFile("sub/bar", []byte("bar0"), 0600),
	)

	st := llb.Scratch().File(
		llb.Copy(
			llb.Local("mylocal"), "/", "/", &llb.CopyInfo{
				IncludePatterns: []string{"sub/*"},
				ExcludePatterns: []string{"sub/bar"},
			},
		),
	)

	busybox := llb.Image("busybox:latest")
	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}
	run(`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal": dir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "sub", "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("foo0"), dt)

	for _, name := range []string{"myfile", "sub/bar"} {
		_, err = os.Stat(filepath.Join(destDir, name))
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	randBytes, err := os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)

	// Create additional file which doesn't match the include pattern, and make
	// sure this doesn't invalidate the cache.

	err = fstest.Apply(fstest.CreateFile("unmatchedfile", []byte("data1"), 0600)).Apply(dir.Name)
	require.NoError(t, err)

	st = llb.Scratch().File(
		llb.Copy(
			llb.Local("mylocal"), "/", "/", &llb.CopyInfo{
				IncludePatterns: []string{"sub/*"},
				ExcludePatterns: []string{"sub/bar"},
			},
		),
	)

	run(`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal": dir,
		},
	}, nil)
	require.NoError(t, err)

	randBytes2, err := os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)

	require.Equal(t, randBytes, randBytes2)
}

func testFileOpCopyAlwaysReplaceExistingDestPaths(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	destDirHostPath := integration.Tmpdir(t,
		fstest.CreateDir("root", 0755),
		fstest.CreateDir("root/overwritedir", 0755),
		fstest.CreateFile("root/overwritedir/subfile", nil, 0755),
		fstest.CreateFile("root/overwritefile", nil, 0755),
		fstest.Symlink("dir", "root/overwritesymlink"),
		fstest.CreateDir("root/dir", 0755),
		fstest.CreateFile("root/dir/dirfile1", nil, 0755),
		fstest.CreateDir("root/dir/overwritesubdir", 0755),
		fstest.CreateFile("root/dir/overwritesubfile", nil, 0755),
		fstest.Symlink("dirfile1", "root/dir/overwritesymlink"),
	)
	destDir := llb.Local("destDir")

	srcDirHostPath := integration.Tmpdir(t,
		fstest.CreateDir("root", 0755),
		fstest.CreateFile("root/overwritedir", nil, 0755),
		fstest.CreateDir("root/overwritefile", 0755),
		fstest.CreateFile("root/overwritefile/foo", nil, 0755),
		fstest.CreateDir("root/overwritesymlink", 0755),
		fstest.CreateDir("root/dir", 0755),
		fstest.CreateFile("root/dir/dirfile2", nil, 0755),
		fstest.CreateFile("root/dir/overwritesubdir", nil, 0755),
		fstest.CreateDir("root/dir/overwritesubfile", 0755),
		fstest.CreateDir("root/dir/overwritesymlink", 0755),
	)
	srcDir := llb.Local("srcDir")

	resultDir := destDir.File(llb.Copy(srcDir, "/", "/", &llb.CopyInfo{
		CopyDirContentsOnly:            true,
		AlwaysReplaceExistingDestPaths: true,
	}))

	def, err := resultDir.Marshal(sb.Context())
	require.NoError(t, err)

	resultDirHostPath := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: resultDirHostPath,
			},
		},
		LocalDirs: map[string]string{
			"destDir": destDirHostPath.Name,
			"srcDir":  srcDirHostPath.Name,
		},
	}, nil)
	require.NoError(t, err)

	err = fstest.CheckDirectoryEqualWithApplier(resultDirHostPath, fstest.Apply(
		fstest.CreateDir("root", 0755),
		fstest.CreateFile("root/overwritedir", nil, 0755),
		fstest.CreateDir("root/overwritefile", 0755),
		fstest.CreateFile("root/overwritefile/foo", nil, 0755),
		fstest.CreateDir("root/overwritesymlink", 0755),
		fstest.CreateDir("root/dir", 0755),
		fstest.CreateFile("root/dir/dirfile1", nil, 0755),
		fstest.CreateFile("root/dir/dirfile2", nil, 0755),
		fstest.CreateFile("root/dir/overwritesubdir", nil, 0755),
		fstest.CreateDir("root/dir/overwritesubfile", 0755),
		fstest.CreateDir("root/dir/overwritesymlink", 0755),
	))
	require.NoError(t, err)
}

// testFileOpInputSwap is a regression test that cache is invalidated when subset of fileop is built
func testFileOpInputSwap(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	base := llb.Scratch().File(llb.Mkfile("/foo", 0600, []byte("foo")))

	src := llb.Scratch().File(llb.Mkfile("/bar", 0600, []byte("bar")))

	st := base.File(llb.Copy(src, "/bar", "/baz"))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// bar does not exist in base but index of all inputs remains the same
	st = base.File(llb.Copy(base, "/bar", "/baz"))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	errStr := integration.UnixOrWindows(
		"bar: no such file",
		"bar: The system cannot find the file specified",
	)
	require.Contains(t, err.Error(), errStr)
}

func testLocalSourceDiffer(t *testing.T, sb integration.Sandbox) {
	for _, d := range []llb.DiffType{llb.DiffNone, llb.DiffMetadata} {
		t.Run(fmt.Sprintf("differ=%s", d), func(t *testing.T) {
			testLocalSourceWithDiffer(t, sb, d)
		})
	}
}

func testLocalSourceWithDiffer(t *testing.T, sb integration.Sandbox, d llb.DiffType) {
	c, err := New(context.TODO(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("foo", []byte("foo"), 0600),
	)

	tv := syscall.NsecToTimespec(time.Now().UnixNano())

	err = syscall.UtimesNano(filepath.Join(dir.Name, "foo"), []syscall.Timespec{tv, tv})
	require.NoError(t, err)

	st := llb.Local("mylocal"+string(d), llb.Differ(d, false))

	def, err := st.Marshal(context.TODO())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(context.TODO(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal" + string(d): dir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("foo"), dt)

	err = os.WriteFile(filepath.Join(dir.Name, "foo"), []byte("bar"), 0600)
	require.NoError(t, err)

	err = syscall.UtimesNano(filepath.Join(dir.Name, "foo"), []syscall.Timespec{tv, tv})
	require.NoError(t, err)

	_, err = c.Solve(context.TODO(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal" + string(d): dir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	if d == llb.DiffMetadata {
		require.Equal(t, []byte("foo"), dt)
	}
	if d == llb.DiffNone {
		require.Equal(t, []byte("bar"), dt)
	}
}

// moby/buildkit#4831
func testLocalSourceWithHardlinksFilter(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(context.TODO(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("bar", []byte("bar"), 0600),
		fstest.Link("bar", "foo1"),
		fstest.Link("bar", "foo2"),
	)

	st := llb.Local("mylocal", llb.FollowPaths([]string{"foo*"}))

	def, err := st.Marshal(context.TODO())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(context.TODO(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal": dir,
		},
	}, nil)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(destDir, "bar"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	dt, err := os.ReadFile(filepath.Join(destDir, "foo1"))
	require.NoError(t, err)
	require.Equal(t, []byte("bar"), dt)

	st1, err := os.Stat(filepath.Join(destDir, "foo1"))
	require.NoError(t, err)

	st2, err := os.Stat(filepath.Join(destDir, "foo2"))
	require.NoError(t, err)

	require.True(t, os.SameFile(st1, st2))
}

func testOCILayoutSource(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureOCILayout)
	c, err := New(context.TODO(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// create a tempdir where we will store the OCI layout
	dir := t.TempDir()

	// make an image that is exported there
	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(
		llb.Scratch(),
		llb.Image(imgName),
	)

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	cmdPrefix := integration.UnixOrWindows(
		`sh -c "echo -n`,
		`cmd /C "echo`,
	)
	run(fmt.Sprintf(`%s first > foo"`, cmdPrefix))
	run(fmt.Sprintf(`%s second > bar"`, cmdPrefix))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	outW := bytes.NewBuffer(nil)
	attrs := map[string]string{}
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Attrs:  attrs,
				Output: fixedWriteCloser(nopWriteCloser{outW}),
			},
		},
	}, nil)
	require.NoError(t, err)

	// extract the tar stream to the directory as OCI layout
	m, err := testutil.ReadTarToMap(outW.Bytes(), false)
	require.NoError(t, err)

	for filename, content := range m {
		fullFilename := path.Join(dir, filename)
		err = os.MkdirAll(path.Dir(fullFilename), 0755)
		require.NoError(t, err)
		if content.Header.FileInfo().IsDir() {
			err = os.MkdirAll(fullFilename, 0755)
			require.NoError(t, err)
		} else {
			err = os.WriteFile(fullFilename, content.Data, 0644)
			require.NoError(t, err)
		}
	}

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)
	require.Equal(t, 1, len(index.Manifests))
	digest := index.Manifests[0].Digest

	store, err := local.NewStore(dir)
	require.NoError(t, err)

	// reference the OCI Layout in a build
	// note that the key does not need to be the directory name, just something
	// unique. since we are doing just one build with one remote here, we can
	// give it any ID
	csID := "my-content-store"
	st = llb.OCILayout(fmt.Sprintf("not/real@%s", digest), llb.OCIStore("", csID))

	def, err = st.Marshal(context.TODO())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(context.TODO(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		OCIStores: map[string]content.Store{
			csID: store,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	newLine := integration.UnixOrWindows("", " \r\n")
	require.Equal(t, []byte("first"+newLine), dt)

	dt, err = os.ReadFile(filepath.Join(destDir, "bar"))
	require.NoError(t, err)
	require.Equal(t, []byte("second"+newLine), dt)
}

func testSessionExporter(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureOCILayout)
	c, err := New(context.TODO(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// make an image that is exported there
	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(
		llb.Scratch(),
		llb.Image(imgName),
	)

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	cmdPrefix := `sh -c "echo -n`
	run(fmt.Sprintf(`%s first > foo"`, cmdPrefix))
	run(fmt.Sprintf(`%s second > bar"`, cmdPrefix))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	var attachable []session.Attachable
	target := filesync.NewFSSyncTarget()

	attachable = append(attachable, target)

	buildID := identity.NewID()

	outW := bytes.NewBuffer(nil)
	exporterCalled := false
	exporter := exporterprovider.New(func(ctx context.Context, md map[string][]byte, refs []string) ([]*exporter.ExporterRequest, error) {
		require.Len(t, refs, 1)
		g := c.GatewayClientForBuild(buildID)
		resp, err := g.ReadDir(sb.Context(), &gatewaypb.ReadDirRequest{
			Ref:     refs[0],
			DirPath: "/",
		})
		if err != nil {
			return nil, err
		}
		t.Logf("readdir: %v", resp)

		require.Equal(t, 2, len(resp.Entries))
		require.Equal(t, "bar", resp.Entries[0].Path)
		require.Equal(t, "foo", resp.Entries[1].Path)

		exporterCalled = true
		target.Add(filesync.WithFSSync(0, fixedWriteCloser(nopWriteCloser{outW})))
		return []*exporter.ExporterRequest{
			{
				Type: ExporterOCI,
				Attrs: map[string]string{
					"compression": "zstd",
				},
			},
		}, nil
	})
	require.NoError(t, err)
	attachable = append(attachable, exporter)

	_, err = c.Build(sb.Context(), SolveOpt{
		EnableSessionExporter: true,
		Session:               attachable,
		Ref:                   buildID,
	}, "", func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}, nil)
	require.NoError(t, err)

	require.True(t, exporterCalled)

	// extract the tar stream to the directory as OCI layout
	m, err := testutil.ReadTarToMap(outW.Bytes(), false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)
	require.Equal(t, 1, len(index.Manifests))
	digest := index.Manifests[0].Digest

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[path.Join("blobs/sha256", digest.Hex())].Data, &mfst)
	require.NoError(t, err)

	if runtime.GOOS == "windows" {
		mfst.Layers = mfst.Layers[1:] // skip base layer
	}

	require.Equal(t, 2, len(mfst.Layers))
	for _, layer := range mfst.Layers {
		require.Contains(t, layer.MediaType, "application/vnd.oci.image.layer.v1.tar+zstd")
	}
}

func testOCILayoutPlatformSource(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureOCILayout)
	requiresLinux(t)
	c, err := New(context.TODO(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// create a tempdir where we will store the OCI layout
	dir := t.TempDir()

	platformsToTest := []string{"linux/amd64", "linux/arm64"}

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(platformsToTest)),
		}
		for i, platform := range platformsToTest {
			st := llb.Scratch().File(
				llb.Mkfile("platform", 0600, []byte(platform)),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(platform, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       platform,
				Platform: platforms.MustParse(platform),
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}
	attrs := map[string]string{}
	outW := bytes.NewBuffer(nil)
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Attrs:  attrs,
				Output: fixedWriteCloser(nopWriteCloser{outW}),
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	// extract the tar stream to the directory as OCI layout
	m, err := testutil.ReadTarToMap(outW.Bytes(), false)
	require.NoError(t, err)

	for filename, tarItem := range m {
		fullFilename := path.Join(dir, filename)
		err = os.MkdirAll(path.Dir(fullFilename), 0755)
		require.NoError(t, err)
		if tarItem.Header.FileInfo().IsDir() {
			err = os.MkdirAll(fullFilename, 0755)
			require.NoError(t, err)
		} else {
			err = os.WriteFile(fullFilename, tarItem.Data, 0644)
			require.NoError(t, err)
		}
	}

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)
	require.Equal(t, 1, len(index.Manifests))
	digest := index.Manifests[0].Digest

	store, err := local.NewStore(dir)
	require.NoError(t, err)
	csID := "my-content-store"

	destDir := t.TempDir()

	frontendOCILayout := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(platformsToTest)),
		}
		for i, platform := range platformsToTest {
			st := llb.OCILayout(fmt.Sprintf("not/real@%s", digest), llb.OCIStore("", csID))

			def, err := st.Marshal(ctx, llb.Platform(platforms.MustParse(platform)))
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(platform, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       platform,
				Platform: platforms.MustParse(platform),
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		OCIStores: map[string]content.Store{
			csID: store,
		},
	}, "", frontendOCILayout, nil)
	require.NoError(t, err)

	for _, platform := range platformsToTest {
		dt, err := os.ReadFile(filepath.Join(destDir, strings.ReplaceAll(platform, "/", "_"), "platform"))
		require.NoError(t, err)
		require.Equal(t, []byte(platform), dt)
	}
}

func testFileOpSymlink(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)

	const (
		fileOwner = 7777
		fileGroup = 8888
		linkOwner = 1111
		linkGroup = 2222

		dummyTimestamp = 42
	)

	dummyTime := time.Unix(dummyTimestamp, 0)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Scratch().
		File(llb.Mkdir("/foo", 0700).Mkfile("bar", 0600, []byte("contents"), llb.ChownOpt{
			User: &llb.UserOpt{
				UID: fileOwner,
			},
			Group: &llb.UserOpt{
				UID: fileGroup,
			},
		})).
		File(llb.Symlink("bar", "/baz", llb.WithCreatedTime(dummyTime), llb.ChownOpt{
			User: &llb.UserOpt{
				UID: linkOwner,
			},
			Group: &llb.UserOpt{
				UID: linkGroup,
			},
		}))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	defer outW.Close()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)
	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	entry, ok := m["bar"]
	require.True(t, ok)

	dt = entry.Data
	header := entry.Header
	require.NoError(t, err)

	require.Equal(t, []byte("contents"), dt)
	require.Equal(t, fileOwner, header.Uid)
	require.Equal(t, fileGroup, header.Gid)

	entry, ok = m["baz"]
	require.Equal(t, true, ok)

	header = entry.Header
	// ensure it is a symlink
	require.Equal(t, tar.TypeSymlink, rune(header.Typeflag))
	// ensure it is a symlink to the proper location
	require.Equal(t, "bar", header.Linkname)

	// make sure it was chowned properly
	require.Equal(t, linkOwner, header.Uid)
	require.Equal(t, linkGroup, header.Gid)

	// ensure it was timestamped properly
	require.Equal(t, dummyTime, header.ModTime)
}

func testFileOpRmWildcard(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dir := integration.Tmpdir(
		t,
		fstest.CreateDir("foo", 0700),
		fstest.CreateDir("bar", 0700),
		fstest.CreateFile("foo/target", []byte("foo0"), 0600),
		fstest.CreateFile("bar/target", []byte("bar0"), 0600),
		fstest.CreateFile("bar/remaining", []byte("bar1"), 0600),
	)

	st := llb.Scratch().File(
		llb.Copy(llb.Local("mylocal"), "foo", "foo").
			Copy(llb.Local("mylocal"), "bar", "bar"),
	).File(
		llb.Rm("*/target", llb.WithAllowWildcard(true)),
	)
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"mylocal": dir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "bar/remaining"))
	require.NoError(t, err)
	require.Equal(t, []byte("bar1"), dt)

	fi, err := os.Stat(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, true, fi.IsDir())

	_, err = os.Stat(filepath.Join(destDir, "foo/target"))
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = os.Stat(filepath.Join(destDir, "bar/target"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func testCallDiskUsage(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.DiskUsage(sb.Context())
	require.NoError(t, err)
}

func testBuildMultiMount(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgAlpine := integration.UnixOrWindows(
		"docker.io/library/alpine:latest",
		"nanoserver",
	)
	imgBusybox := integration.UnixOrWindows(
		"docker.io/library/busybox:latest",
		"nanoserver",
	)
	cmdStr1 := integration.UnixOrWindows(
		"/bin/ls -l",
		"cmd /C dir",
	)
	cmdStr2 := integration.UnixOrWindows(
		"/bin/cp -a /busybox/etc/passwd baz",
		`cmd /C copy C:\\busybox\\License.txt baz`,
	)

	alpine := llb.Image(imgAlpine)
	ls := alpine.Run(llb.Shlex(cmdStr1))
	busybox := llb.Image(imgBusybox)
	cp := ls.Run(llb.Shlex(cmdStr2))
	cp.AddMount("/busybox", busybox)

	def, err := cp.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)
}

func testBuildExportScratch(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureImageExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	makeFrontend := func(ps []string) func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		return func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
			st := llb.Scratch()
			def, err := st.Marshal(sb.Context())
			require.NoError(t, err)

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			res := gateway.NewResult()
			if ps == nil {
				res.SetRef(ref)
			} else {
				for _, p := range ps {
					res.AddRef(p, ref)
				}

				expPlatforms := &exptypes.Platforms{
					Platforms: make([]exptypes.Platform, len(ps)),
				}
				for i, pk := range ps {
					p := platforms.MustParse(pk)

					img := ocispecs.Image{
						Platform: p,
						Config: ocispecs.ImageConfig{
							Labels: map[string]string{
								"foo": "i am platform " + platforms.Format(p),
							},
						},
					}
					config, err := json.Marshal(img)
					if err != nil {
						return nil, errors.Wrapf(err, "failed to marshal image config")
					}
					res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, pk), config)

					expPlatforms.Platforms[i] = exptypes.Platform{
						ID:       pk,
						Platform: p,
					}
				}
				dt, err := json.Marshal(expPlatforms)
				if err != nil {
					return nil, err
				}
				res.AddMeta(exptypes.ExporterPlatformsKey, dt)
			}

			return res, nil
		}
	}

	target := registry + "/buildkit/build/exporter:withnocompressed"

	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              target,
					"push":              "true",
					"unpack":            "true",
					"compression":       "uncompressed",
					"attest:provenance": "mode=max",
				},
			},
		},
	}, "", makeFrontend(nil), nil)
	require.NoError(t, err)

	targetMulti := registry + "/buildkit/build/exporter-multi:withnocompressed"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              targetMulti,
					"push":              "true",
					"unpack":            "true",
					"compression":       "uncompressed",
					"attest:provenance": "mode=max",
				},
			},
		},
	}, "", makeFrontend([]string{"linux/amd64", "linux/arm64"}), nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Len(t, imgs.Images, 1)
	img := imgs.Find(platforms.Format(platforms.DefaultSpec()))
	require.Empty(t, img.Layers)
	require.Equal(t, platforms.DefaultSpec(), img.Img.Platform)

	desc, provider, err = contentutil.ProviderFromRef(targetMulti)
	require.NoError(t, err)
	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Len(t, imgs.Images, 2)
	img = imgs.Find("linux/amd64")
	require.Empty(t, img.Layers)
	require.Equal(t, "linux/amd64", platforms.Format(img.Img.Platform))
	require.Equal(t, "i am platform linux/amd64", img.Img.Config.Labels["foo"])
	img = imgs.Find("linux/arm64")
	require.Empty(t, img.Layers)
	require.Equal(t, "linux/arm64", platforms.Format(img.Img.Platform))
	require.Equal(t, "i am platform linux/arm64", img.Img.Config.Labels["foo"])
}

func testBuildHTTPSource(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid falso positive with current time

	resp := httpserver.Response{
		Etag:         identity.NewID(),
		Content:      []byte("content1"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/foo": resp,
	})
	defer server.Close()

	// invalid URL first
	st := llb.HTTP(server.URL + "/bar")

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid response status 404")

	// first correct request
	st = llb.HTTP(server.URL + "/foo")

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	require.Equal(t, 1, server.Stats("/foo").AllRequests)
	require.Equal(t, 0, server.Stats("/foo").CachedRequests)

	tmpdir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: tmpdir,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 2, server.Stats("/foo").AllRequests)
	require.Equal(t, 1, server.Stats("/foo").CachedRequests)

	dt, err := os.ReadFile(filepath.Join(tmpdir, "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("content1"), dt)

	allReqs := server.Stats("/foo").Requests
	require.Equal(t, 2, len(allReqs))
	require.Equal(t, http.MethodGet, allReqs[0].Method)
	require.Equal(t, "gzip", allReqs[0].Header.Get("Accept-Encoding"))
	require.Equal(t, http.MethodHead, allReqs[1].Method)
	require.Equal(t, "gzip", allReqs[1].Header.Get("Accept-Encoding"))

	require.NoError(t, os.RemoveAll(filepath.Join(tmpdir, "foo")))

	// update the content at the url to be gzipped now, the final output
	// should remain the same
	modTime = time.Now().Add(-23 * time.Hour)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err = gw.Write(resp.Content)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	gzipBytes := buf.Bytes()
	respGzip := httpserver.Response{
		Etag:            identity.NewID(),
		Content:         gzipBytes,
		LastModified:    &modTime,
		ContentEncoding: "gzip",
	}
	server.SetRoute("/foo", respGzip)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: tmpdir,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 4, server.Stats("/foo").AllRequests)
	require.Equal(t, 1, server.Stats("/foo").CachedRequests)

	dt, err = os.ReadFile(filepath.Join(tmpdir, "foo"))
	require.NoError(t, err)
	require.Equal(t, resp.Content, dt)

	allReqs = server.Stats("/foo").Requests
	require.Equal(t, 4, len(allReqs))
	require.Equal(t, http.MethodHead, allReqs[2].Method)
	require.Equal(t, "gzip", allReqs[2].Header.Get("Accept-Encoding"))
	require.Equal(t, http.MethodGet, allReqs[3].Method)
	require.Equal(t, "gzip", allReqs[3].Header.Get("Accept-Encoding"))

	// test extra options
	// llb.Chown not supported on Windows
	st = integration.UnixOrWindows(
		llb.HTTP(server.URL+"/foo", llb.Filename("bar"), llb.Chmod(0741), llb.Chown(1000, 1000)),
		llb.HTTP(server.URL+"/foo", llb.Filename("bar"), llb.Chmod(0741)),
	)
	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: tmpdir,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 5, server.Stats("/foo").AllRequests)
	require.Equal(t, 1, server.Stats("/foo").CachedRequests)

	dt, err = os.ReadFile(filepath.Join(tmpdir, "bar"))
	require.NoError(t, err)
	require.Equal(t, []byte("content1"), dt)

	fi, err := os.Stat(filepath.Join(tmpdir, "bar"))
	require.NoError(t, err)
	require.Equal(t, fi.ModTime().Format(http.TimeFormat), modTime.Format(http.TimeFormat))

	// no support for llb.Chmod on Windows, default is returned
	fMode := integration.UnixOrWindows(0741, 0666)
	require.Equal(t, fMode, int(fi.Mode()&0777))

	checkAllReleasable(t, c, sb, true)

	// TODO: check that second request was marked as cached
}

// docker/buildx#2803
func testBuildHTTPSourceEtagScope(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid falso positive with current time

	sharedEtag := identity.NewID()
	resp := httpserver.Response{
		Etag:         sharedEtag,
		Content:      []byte("content1"),
		LastModified: &modTime,
	}
	resp2 := httpserver.Response{
		Etag:         sharedEtag,
		Content:      []byte("another"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/one/foo": resp,
		"/two/foo": resp2,
	})
	defer server.Close()

	// first correct request
	st := llb.HTTP(server.URL + "/one/foo")

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	out1 := t.TempDir()
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: out1,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 1, server.Stats("/one/foo").AllRequests)
	require.Equal(t, 0, server.Stats("/one/foo").CachedRequests)

	dt, err := os.ReadFile(filepath.Join(out1, "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("content1"), dt)

	st = llb.HTTP(server.URL + "/two/foo")

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	out2 := t.TempDir()
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: out2,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 1, server.Stats("/two/foo").AllRequests)
	require.Equal(t, 0, server.Stats("/two/foo").CachedRequests)

	dt, err = os.ReadFile(filepath.Join(out2, "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("another"), dt)

	out2 = t.TempDir()
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: out2,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 2, server.Stats("/two/foo").AllRequests)
	require.Equal(t, 1, server.Stats("/two/foo").CachedRequests)

	allReqs := server.Stats("/two/foo").Requests
	require.Equal(t, 2, len(allReqs))
	require.Equal(t, http.MethodGet, allReqs[0].Method)
	require.Equal(t, "gzip", allReqs[0].Header.Get("Accept-Encoding"))
	require.Equal(t, http.MethodHead, allReqs[1].Method)
	require.Equal(t, "gzip", allReqs[1].Header.Get("Accept-Encoding"))

	require.NoError(t, os.RemoveAll(filepath.Join(out2, "foo")))
}

func testBuildHTTPSourceAuthHeaderSecret(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid false positive with current time

	resp := httpserver.Response{
		Etag:         identity.NewID(),
		Content:      []byte("content1"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/foo": resp,
	})
	defer server.Close()

	st := llb.HTTP(server.URL+"/foo", llb.AuthHeaderSecret("http-secret"))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(
		sb.Context(),
		def,
		SolveOpt{
			Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
				"http-secret": []byte("Bearer foo"),
			})},
		},
		nil,
	)
	require.NoError(t, err)

	allReqs := server.Stats("/foo").Requests
	require.Equal(t, 1, len(allReqs))
	require.Equal(t, http.MethodGet, allReqs[0].Method)
	require.Equal(t, "Bearer foo", allReqs[0].Header.Get("Authorization"))
}

func testBuildHTTPSourceHostTokenSecret(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid false positive with current time

	resp := httpserver.Response{
		Etag:         identity.NewID(),
		Content:      []byte("content1"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/foo": resp,
	})
	defer server.Close()

	st := llb.HTTP(server.URL + "/foo")

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(
		sb.Context(),
		def,
		SolveOpt{
			Session: []session.Attachable{secretsprovider.FromMap(map[string][]byte{
				"HTTP_AUTH_TOKEN_127.0.0.1": []byte("123456"),
			})},
		},
		nil,
	)
	require.NoError(t, err)

	allReqs := server.Stats("/foo").Requests
	require.Equal(t, 1, len(allReqs))
	require.Equal(t, http.MethodGet, allReqs[0].Method)
	require.Equal(t, "Bearer 123456", allReqs[0].Header.Get("Authorization"))
}

func testBuildHTTPSourceHeader(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid falso positive with current time

	resp := httpserver.Response{
		Etag:         identity.NewID(),
		Content:      []byte("content1"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/foo": resp,
	})
	defer server.Close()

	st := llb.HTTP(
		server.URL+"/foo",
		llb.Header(llb.HTTPHeader{
			Accept:    "application/vnd.foo",
			UserAgent: "fooagent",
		}),
	)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	allReqs := server.Stats("/foo").Requests
	require.Equal(t, 1, len(allReqs))
	require.Equal(t, http.MethodGet, allReqs[0].Method)
	require.Equal(t, "application/vnd.foo", allReqs[0].Header.Get("accept"))
	require.Equal(t, "fooagent", allReqs[0].Header.Get("user-agent"))
}

func testResolveAndHosts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "cp /etc/resolv.conf ."`)
	run(`sh -c "cp /etc/hosts ."`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "resolv.conf"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "nameserver")

	dt, err = os.ReadFile(filepath.Join(destDir, "hosts"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "127.0.0.1	localhost")
}

func testUser(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").Run(llb.Shlex(`sh -c "mkdir -m 0777 /wd"`))

	run := func(user, cmd string) {
		if user != "" {
			st = st.Run(llb.Shlex(cmd), llb.Dir("/wd"), llb.User(user))
		} else {
			st = st.Run(llb.Shlex(cmd), llb.Dir("/wd"))
		}
	}

	run("daemon", `sh -c "id -nu > user"`)
	run("daemon:daemon", `sh -c "id -ng > group"`)
	run("daemon:nobody", `sh -c "id -ng > nobody"`)
	run("1:1", `sh -c "id -g > userone"`)
	run("root", `sh -c "id -Gn > root_supplementary"`)
	run("", `sh -c "id -Gn > default_supplementary"`)
	run("", `rm /etc/passwd /etc/group`) // test that default user still works
	run("", `sh -c "id -u > default_uid"`)

	st = st.Run(llb.Shlex("cp -a /wd/. /out/"))
	out := st.AddMount("/out", llb.Scratch())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "user"))
	require.NoError(t, err)
	require.Equal(t, "daemon", strings.TrimSpace(string(dt)))

	dt, err = os.ReadFile(filepath.Join(destDir, "group"))
	require.NoError(t, err)
	require.Equal(t, "daemon", strings.TrimSpace(string(dt)))

	dt, err = os.ReadFile(filepath.Join(destDir, "nobody"))
	require.NoError(t, err)
	require.Equal(t, "nobody", strings.TrimSpace(string(dt)))

	dt, err = os.ReadFile(filepath.Join(destDir, "userone"))
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(string(dt)))

	dt, err = os.ReadFile(filepath.Join(destDir, "root_supplementary"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(dt), "root "))
	require.Contains(t, string(dt), "wheel")

	dt2, err := os.ReadFile(filepath.Join(destDir, "default_supplementary"))
	require.NoError(t, err)
	require.Equal(t, string(dt), string(dt2))

	dt, err = os.ReadFile(filepath.Join(destDir, "default_uid"))
	require.NoError(t, err)
	require.Equal(t, "0", strings.TrimSpace(string(dt)))

	checkAllReleasable(t, c, sb, true)
}

func testMultipleExporters(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	def, err := llb.Scratch().File(llb.Mkfile("foo.txt", 0o755, nil)).Marshal(context.TODO())
	require.NoError(t, err)

	destDir, destDir2 := t.TempDir(), t.TempDir()
	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	defer outW.Close()

	out2 := filepath.Join(destDir, "out2.tar")
	outW2, err := os.Create(out2)
	require.NoError(t, err)
	defer outW2.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target1, target2 := registry+"/buildkit/build/exporter:image",
		registry+"/buildkit/build/alternative:image"

	var exporters []ExportEntry
	if workers.IsTestDockerd() {
		exporters = append(exporters, ExportEntry{
			Type: "moby",
			Attrs: map[string]string{
				"name": strings.Join([]string{target1, target2}, ","),
			},
		})
	} else {
		exporters = append(exporters, []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target1,
				},
			},
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":           target2,
					"oci-mediatypes": "true",
				},
			},
		}...)
	}

	exporters = append(exporters, []ExportEntry{
		// Ensure that multiple local exporter destinations are written properly
		{
			Type:      ExporterLocal,
			OutputDir: destDir,
		},
		{
			Type:      ExporterLocal,
			OutputDir: destDir2,
		},
		// Ensure that multiple instances of the same exporter are possible
		{
			Type:   ExporterTar,
			Output: fixedWriteCloser(outW),
		},
		{
			Type:   ExporterTar,
			Output: fixedWriteCloser(outW2),
		},
	}...)

	ref := identity.NewID()
	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Ref:     ref,
		Exports: exporters,
	}, nil)
	require.NoError(t, err)

	if workers.IsTestDockerd() {
		require.Equal(t, target1+","+target2, resp.ExporterResponse[exptypes.ExporterImageNameKey])
	} else {
		require.Equal(t, target2, resp.ExporterResponse[exptypes.ExporterImageNameKey])
	}
	require.FileExists(t, filepath.Join(destDir, "out.tar"))
	require.FileExists(t, filepath.Join(destDir, "out2.tar"))
	require.FileExists(t, filepath.Join(destDir, "foo.txt"))
	require.FileExists(t, filepath.Join(destDir2, "foo.txt"))

	history, err := c.ControlClient().ListenBuildHistory(sb.Context(), &controlapi.BuildHistoryRequest{
		Ref:       ref,
		EarlyExit: true,
	})
	require.NoError(t, err)
	for {
		ev, err := history.Recv()
		if err != nil {
			require.Equal(t, io.EOF, err)
			break
		}
		require.Equal(t, ref, ev.Record.Ref)

		if workers.IsTestDockerd() {
			require.Len(t, ev.Record.Result.Results, 1)
			require.Len(t, ev.Record.Exporters, 5)
			if workers.IsTestDockerdMoby(sb) {
				require.Equal(t, images.MediaTypeDockerSchema2Config, ev.Record.Result.Results[0].MediaType)
			} else {
				require.Equal(t, images.MediaTypeDockerSchema2Manifest, ev.Record.Result.Results[0].MediaType)
			}
		} else {
			require.Len(t, ev.Record.Result.Results, 2)
			require.Len(t, ev.Record.Exporters, 6)
			require.Equal(t, images.MediaTypeDockerSchema2Manifest, ev.Record.Result.Results[0].MediaType)
			require.Equal(t, ocispecs.MediaTypeImageManifest, ev.Record.Result.Results[1].MediaType)
		}
		require.Equal(t, ev.Record.Result.Results[0], ev.Record.Result.ResultDeprecated)
	}
}

func testOCIExporter(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	for _, exp := range []string{ExporterOCI, ExporterDocker} {
		destDir := t.TempDir()

		out := filepath.Join(destDir, "out.tar")
		outW, err := os.Create(out)
		require.NoError(t, err)
		target := "example.com/buildkit/testoci:latest"
		attrs := map[string]string{}
		if exp == ExporterDocker {
			attrs["name"] = target
		}
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:   exp,
					Attrs:  attrs,
					Output: fixedWriteCloser(outW),
				},
			},
		}, nil)
		require.NoError(t, err)

		dt, err := os.ReadFile(out)
		require.NoError(t, err)

		m, err := testutil.ReadTarToMap(dt, false)
		require.NoError(t, err)

		_, ok := m["oci-layout"]
		require.True(t, ok)

		var index ocispecs.Index
		err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
		require.NoError(t, err)
		require.Equal(t, 2, index.SchemaVersion)
		require.Equal(t, 1, len(index.Manifests))

		var mfst ocispecs.Manifest
		err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
		require.NoError(t, err)
		require.Equal(t, 2, len(mfst.Layers))

		var ociimg ocispecs.Image
		err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+mfst.Config.Digest.Hex()].Data, &ociimg)
		require.NoError(t, err)
		require.Equal(t, "layers", ociimg.RootFS.Type)
		require.Equal(t, 2, len(ociimg.RootFS.DiffIDs))

		_, ok = m[ocispecs.ImageBlobsDir+"/sha256/"+mfst.Layers[0].Digest.Hex()]
		require.True(t, ok)
		_, ok = m[ocispecs.ImageBlobsDir+"/sha256/"+mfst.Layers[1].Digest.Hex()]
		require.True(t, ok)

		if exp != ExporterDocker {
			continue
		}

		var dockerMfst []struct {
			Config   string
			RepoTags []string
			Layers   []string
		}
		err = json.Unmarshal(m["manifest.json"].Data, &dockerMfst)
		require.NoError(t, err)
		require.Equal(t, 1, len(dockerMfst))

		_, ok = m[dockerMfst[0].Config]
		require.True(t, ok)
		require.Equal(t, 2, len(dockerMfst[0].Layers))
		require.Equal(t, 1, len(dockerMfst[0].RepoTags))
		require.Equal(t, target, dockerMfst[0].RepoTags[0])

		for _, l := range dockerMfst[0].Layers {
			_, ok := m[l]
			require.True(t, ok)
		}
	}

	checkAllReleasable(t, c, sb, true)
}

func testOCIExporterContentStore(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	for _, exp := range []string{ExporterOCI, ExporterDocker} {
		destDir := t.TempDir()
		target := "example.com/buildkit/testoci:latest"

		outTar := filepath.Join(destDir, "out.tar")
		outW, err := os.Create(outTar)
		require.NoError(t, err)
		attrs := map[string]string{}
		if exp == ExporterDocker {
			attrs["name"] = target
		}
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:   exp,
					Attrs:  attrs,
					Output: fixedWriteCloser(outW),
				},
			},
		}, nil)
		require.NoError(t, err)

		dt, err := os.ReadFile(outTar)
		require.NoError(t, err)
		m, err := testutil.ReadTarToMap(dt, false)
		require.NoError(t, err)

		checkStore := func(dir string) {
			err = filepath.Walk(dir, func(filename string, fi os.FileInfo, err error) error {
				filename = strings.TrimPrefix(filename, dir)
				filename = strings.Trim(filename, "/")
				if filename == "" || filename == "ingest" {
					return nil
				}

				if fi.IsDir() {
					require.Contains(t, m, filename+"/")
				} else {
					require.Contains(t, m, filename)
					if filename == ocispecs.ImageIndexFile {
						// this file has a timestamp in it, so we can't compare
						return nil
					}
					f, err := os.Open(path.Join(dir, filename))
					require.NoError(t, err)
					data, err := io.ReadAll(f)
					require.NoError(t, err)
					require.Equal(t, m[filename].Data, data)
				}
				return nil
			})
			require.NoError(t, err)
		}

		outDir := filepath.Join(destDir, "out.d")
		attrs = map[string]string{
			"tar": "false",
		}
		if exp == ExporterDocker {
			attrs["name"] = target
		}
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:      exp,
					Attrs:     attrs,
					OutputDir: outDir,
				},
			},
		}, nil)
		require.NoError(t, err)
		checkStore(outDir)

		outStoreDir := filepath.Join(destDir, "store.d")
		store, err := local.NewStore(outStoreDir)
		require.NoError(t, err)
		attrs = map[string]string{
			"tar": "false",
		}
		if exp == ExporterDocker {
			attrs["name"] = target
		}
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:        exp,
					Attrs:       attrs,
					OutputStore: store,
				},
			},
		}, nil)
		require.NoError(t, err)
		checkStore(outDir)
	}

	checkAllReleasable(t, c, sb, true)
}

func testNoTarOCIIndexMediaType(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	cmdStr := integration.UnixOrWindows(
		`sh -c "echo -n hello > hello"`,
		`cmd /C "echo hello> hello"`,
	)
	st := llb.Image(imgName).Run(llb.Shlex(cmdStr))
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	outDir := filepath.Join(destDir, "out.d")
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterOCI,
				Attrs: map[string]string{
					"tar": "false",
				},
				OutputDir: outDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(outDir, "index.json"))
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(dt, &index)
	require.NoError(t, err)

	require.Equal(t, "application/vnd.oci.image.index.v1+json", index.MediaType)

	checkAllReleasable(t, c, sb, true)
}

func testOCIIndexMediatype(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	cmdStr := integration.UnixOrWindows(
		`sh -c "echo -n hello > hello"`,
		`cmd /C "echo hello> hello"`,
	)
	st := llb.Image(imgName).Run(llb.Shlex(cmdStr))
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	indexDt, ok := m[ocispecs.ImageIndexFile]
	require.True(t, ok)

	var index ocispecs.Index
	err = json.Unmarshal(indexDt.Data, &index)
	require.NoError(t, err)

	require.Equal(t, "application/vnd.oci.image.index.v1+json", index.MediaType)

	checkAllReleasable(t, c, sb, true)
}

func testSourceDateEpochLayerTimestamps(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	tm := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", tm.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	tmsX, err := readImageTimestamps(dt)
	require.NoError(t, err)
	tms := tmsX.FromImage

	require.Equal(t, 3, len(tms))

	expected := tm.UTC().Format(time.RFC3339Nano)
	require.Equal(t, expected, tms[0])
	require.Equal(t, expected, tms[1])
	require.Equal(t, expected, tms[2])

	checkAllReleasable(t, c, sb, true)
}

func testSourceDateEpochClamp(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	var bboxConfig []byte
	_, err = c.Build(sb.Context(), SolveOpt{}, "", func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		_, _, bboxConfig, err = c.ResolveImageConfig(ctx, "docker.io/library/busybox:latest", sourceresolver.Opt{})
		if err != nil {
			return nil, err
		}
		return nil, nil
	}, nil)
	require.NoError(t, err)

	m := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(bboxConfig, &m))
	delete(m, "created")
	bboxConfig, err = json.Marshal(m)
	require.NoError(t, err)

	busybox, err := llb.Image("busybox:latest").WithImageConfig(bboxConfig)
	require.NoError(t, err)

	def, err := busybox.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterOCI,
				Attrs: map[string]string{
					exptypes.ExporterImageConfigKey: string(bboxConfig),
				},
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	busyboxTmsX, err := readImageTimestamps(dt)
	require.NoError(t, err)
	busyboxTms := busyboxTmsX.FromImage

	require.Greater(t, len(busyboxTms), 1)
	bboxLayerLen := len(busyboxTms) - 1

	tm, err := time.Parse(time.RFC3339Nano, busyboxTms[1])
	require.NoError(t, err)

	next := tm.Add(time.Hour).Truncate(time.Second)

	st := busybox.Run(llb.Shlex("touch /foo"))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	out = filepath.Join(destDir, "out.tar")
	outW, err = os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", next.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type: ExporterOCI,
				Attrs: map[string]string{
					exptypes.ExporterImageConfigKey: string(bboxConfig),
				},
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(out)
	require.NoError(t, err)

	tmsX, err := readImageTimestamps(dt)
	require.NoError(t, err)
	tms := tmsX.FromImage

	require.Equal(t, len(tms), bboxLayerLen+2)

	expected := next.UTC().Format(time.RFC3339Nano)
	require.Equal(t, expected, tms[0])
	require.Equal(t, busyboxTms[1], tms[1])
	require.Equal(t, expected, tms[bboxLayerLen+1])
	require.Equal(t, expected, tmsX.FromAnnotation)

	checkAllReleasable(t, c, sb, true)
}

// testSourceDateEpochReset tests that the SOURCE_DATE_EPOCH is reset if exporter option is set
func testSourceDateEpochReset(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	tm := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", tm.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Attrs:  map[string]string{"source-date-epoch": ""},
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	tmsX, err := readImageTimestamps(dt)
	require.NoError(t, err)
	tms := tmsX.FromImage

	require.Equal(t, 3, len(tms))

	expected := tm.UTC().Format(time.RFC3339Nano)
	require.NotEqual(t, expected, tms[0])
	require.NotEqual(t, expected, tms[1])
	require.NotEqual(t, expected, tms[2])

	require.Equal(t, tms[0], tms[2])
	require.NotEqual(t, tms[2], tms[1])

	checkAllReleasable(t, c, sb, true)
}

func testSourceDateEpochLocalExporter(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	tm := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", tm.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	fi, err := os.Stat(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, fi.ModTime().Format(time.RFC3339), tm.UTC().Format(time.RFC3339))

	fi, err = os.Stat(filepath.Join(destDir, "bar"))
	require.NoError(t, err)
	require.Equal(t, fi.ModTime().Format(time.RFC3339), tm.UTC().Format(time.RFC3339))

	checkAllReleasable(t, c, sb, true)
}

func testSourceDateEpochTarExporter(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir, err := os.MkdirTemp("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	tm := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", tm.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	require.Equal(t, 2, len(m))

	require.Equal(t, tm.Format(time.RFC3339), m["foo"].Header.ModTime.Format(time.RFC3339))
	require.Equal(t, tm.Format(time.RFC3339), m["bar"].Header.ModTime.Format(time.RFC3339))

	checkAllReleasable(t, c, sb, true)
}

func testSourceDateEpochImageExporter(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.SkipNow()
	}

	// https://github.com/containerd/containerd/commit/133ddce7cf18a1db175150e7a69470dea1bb3132
	minVer := "v1.7.0-beta.1"
	cdVersion := containerdutil.GetVersion(t, cdAddress)
	if semver.Compare(cdVersion, minVer) < 0 {
		t.Skipf("containerd version %q does not satisfy minimal version %q", cdVersion, minVer)
	}

	workers.CheckFeatureCompat(t, sb, workers.FeatureSourceDateEpoch)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	name := strings.ToLower(path.Base(t.Name()))
	tm := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"build-arg:SOURCE_DATE_EPOCH": fmt.Sprintf("%d", tm.Unix()),
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": name,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	ctx := namespaces.WithNamespace(sb.Context(), "buildkit")
	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	img, err := client.GetImage(ctx, name)
	require.NoError(t, err)
	require.Equal(t, tm, img.Metadata().CreatedAt)

	err = client.ImageService().Delete(ctx, name, images.SynchronousDelete())
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)
}

func testFrontendMetadataReturn(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		res.AddMeta("frontend.returned", []byte("true"))
		res.AddMeta("not-frontend.not-returned", []byte("false"))
		res.AddMeta("frontendnot.returned.either", []byte("false"))
		return res, nil
	}

	var exports []ExportEntry
	if workers.IsTestDockerdMoby(sb) {
		exports = []ExportEntry{{
			Type: "moby",
			Attrs: map[string]string{
				"name": "reg.dummy:5000/buildkit/test:latest",
			},
		}}
	} else {
		exports = []ExportEntry{{
			Type:   ExporterOCI,
			Attrs:  map[string]string{},
			Output: fixedWriteCloser(nopWriteCloser{io.Discard}),
		}}
	}

	res, err := c.Build(sb.Context(), SolveOpt{
		Exports: exports,
	}, "", frontend, nil)
	require.NoError(t, err)
	require.Contains(t, res.ExporterResponse, "frontend.returned")
	require.Equal(t, "true", res.ExporterResponse["frontend.returned"])
	require.NotContains(t, res.ExporterResponse, "not-frontend.not-returned")
	require.NotContains(t, res.ExporterResponse, "frontendnot.returned.either")
	checkAllReleasable(t, c, sb, true)
}

func testFrontendUseSolveResults(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.Scratch().File(
			llb.Mkfile("foo", 0600, []byte("data")),
		)

		def, err := st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		res, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}

		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}

		st2, err := ref.ToState()
		if err != nil {
			return nil, err
		}

		st = llb.Scratch().File(
			llb.Copy(st2, "foo", "foo2"),
		)

		def, err = st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}

	destDir := t.TempDir()

	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo2"))
	require.NoError(t, err)
	require.Equal(t, dt, []byte("data"))
}

func testExporterTargetExists(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	st := llb.Image(imgName)
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	var mdDgst string
	res, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:  ExporterOCI,
				Attrs: map[string]string{},
				Output: func(m map[string]string) (io.WriteCloser, error) {
					mdDgst = m[exptypes.ExporterImageDigestKey]
					return nil, nil
				},
			},
		},
	}, nil)
	require.NoError(t, err)
	dgst := res.ExporterResponse[exptypes.ExporterImageDigestKey]

	require.True(t, strings.HasPrefix(dgst, "sha256:"))
	require.Equal(t, dgst, mdDgst)

	require.True(t, strings.HasPrefix(res.ExporterResponse[exptypes.ExporterImageConfigDigestKey], "sha256:"))
}

func testTarExporterWithSocket(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	alpine := llb.Image("docker.io/library/alpine:latest")
	def, err := alpine.Run(llb.Args([]string{"sh", "-c", "nc -l -s local:/socket.sock & usleep 100000; kill %1"})).Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:  ExporterTar,
				Attrs: map[string]string{},
				Output: func(m map[string]string) (io.WriteCloser, error) {
					return nopWriteCloser{io.Discard}, nil
				},
			},
		},
	}, nil)
	require.NoError(t, err)
}

func testTarExporterWithSocketCopy(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	alpine := llb.Image("docker.io/library/alpine:latest")
	state := alpine.Run(llb.Args([]string{"sh", "-c", "nc -l -s local:/root/socket.sock & usleep 100000; kill %1"})).Root()

	fa := llb.Copy(state, "/root", "/roo2", &llb.CopyInfo{})

	scratchCopy := llb.Scratch().File(fa)

	def, err := scratchCopy.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

// moby/buildkit#1418
func testTarExporterSymlink(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	cmdStr := integration.UnixOrWindows(
		`sh -c "echo -n first > foo;ln -s foo bar"`,
		`cmd /C "echo first> foo && mklink bar foo"`,
	)
	run(cmdStr)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(&nopWriteCloser{&buf}),
			},
		},
	}, nil)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(buf.Bytes(), false)
	require.NoError(t, err)

	item, ok := m["foo"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	newLine := integration.UnixOrWindows("", " \r\n")
	require.Equal(t, []byte("first"+newLine), item.Data)

	item, ok = m["bar"]
	require.True(t, ok)
	require.Equal(t, tar.TypeSymlink, int32(item.Header.Typeflag))
	require.Equal(t, "foo", item.Header.Linkname)
}

func testBuildExportWithForeignLayer(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureImageExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("cpuguy83/buildkit-foreign:latest")
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	t.Run("propagate=1", func(t *testing.T) {
		registry, err := sb.NewRegistry()
		if errors.Is(err, integration.ErrRequirements) {
			t.Skip(err.Error())
		}
		require.NoError(t, err)

		target := registry + "/buildkit/build/exporter/foreign:latest"
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type: ExporterImage,
					Attrs: map[string]string{
						"name":                  target,
						"push":                  "true",
						"prefer-nondist-layers": "true",
					},
				},
			},
		}, nil)
		require.NoError(t, err)

		ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

		resolver := docker.NewResolver(docker.ResolverOptions{PlainHTTP: true})
		name, desc, err := resolver.Resolve(ctx, target)
		require.NoError(t, err)

		fetcher, err := resolver.Fetcher(ctx, name)
		require.NoError(t, err)
		mfst, err := images.Manifest(ctx, contentutil.FromFetcher(fetcher), desc, platforms.Any())
		require.NoError(t, err)

		require.Equal(t, 2, len(mfst.Layers))
		require.Equal(t, images.MediaTypeDockerSchema2LayerForeign, mfst.Layers[0].MediaType)
		require.Len(t, mfst.Layers[0].URLs, 1)
		require.Equal(t, images.MediaTypeDockerSchema2Layer, mfst.Layers[1].MediaType)

		rc, err := fetcher.Fetch(ctx, ocispecs.Descriptor{Digest: mfst.Layers[0].Digest, Size: mfst.Layers[0].Size})
		require.NoError(t, err)
		defer rc.Close()

		// `Fetch` doesn't error (in the docker resolver), it just returns a reader immediately and does not make a request.
		// The request is only made when we attempt to read from the reader.
		buf := make([]byte, 1)
		_, err = rc.Read(buf)
		require.Truef(t, cerrdefs.IsNotFound(err), "expected error for blob that should not be in registry: %s, %v", mfst.Layers[0].Digest, err)
	})
	t.Run("propagate=0", func(t *testing.T) {
		registry, err := sb.NewRegistry()
		if errors.Is(err, integration.ErrRequirements) {
			t.Skip(err.Error())
		}
		require.NoError(t, err)
		target := registry + "/buildkit/build/exporter/noforeign:latest"
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type: ExporterImage,
					Attrs: map[string]string{
						"name": target,
						"push": "true",
					},
				},
			},
		}, nil)
		require.NoError(t, err)

		ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

		resolver := docker.NewResolver(docker.ResolverOptions{PlainHTTP: true})
		name, desc, err := resolver.Resolve(ctx, target)
		require.NoError(t, err)

		fetcher, err := resolver.Fetcher(ctx, name)
		require.NoError(t, err)

		mfst, err := images.Manifest(ctx, contentutil.FromFetcher(fetcher), desc, platforms.Any())
		require.NoError(t, err)

		require.Equal(t, 2, len(mfst.Layers))
		require.Equal(t, images.MediaTypeDockerSchema2Layer, mfst.Layers[0].MediaType)
		require.Len(t, mfst.Layers[0].URLs, 0)
		require.Equal(t, images.MediaTypeDockerSchema2Layer, mfst.Layers[1].MediaType)

		rc, err := fetcher.Fetch(ctx, ocispecs.Descriptor{Digest: mfst.Layers[0].Digest, Size: mfst.Layers[0].Size})
		require.NoError(t, err)
		defer rc.Close()

		// `Fetch` doesn't error (in the docker resolver), it just returns a reader immediately and does not make a request.
		// The request is only made when we attempt to read from the reader.
		buf := make([]byte, 1)
		_, err = rc.Read(buf)
		require.NoError(t, err)
	})
}

func testBuildExportWithUncompressed(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureImageExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	cmd := `sh -e -c "echo -n uncompressed > data"`

	st := llb.Scratch()
	st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/build/exporter:withnocompressed"

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":        target,
					"push":        "true",
					"compression": "uncompressed",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	ctx := namespaces.WithNamespace(sb.Context(), "buildkit")
	cdAddress := sb.ContainerdAddress()
	var client *ctd.Client
	if cdAddress != "" {
		client, err = newContainerd(cdAddress)
		require.NoError(t, err)
		defer client.Close()

		img, err := client.GetImage(ctx, target)
		require.NoError(t, err)
		mfst, err := images.Manifest(ctx, client.ContentStore(), img.Target(), nil)
		require.NoError(t, err)
		require.Equal(t, 1, len(mfst.Layers))
		require.Equal(t, images.MediaTypeDockerSchema2Layer, mfst.Layers[0].MediaType)
	}

	// new layer with gzip compression
	targetImg := llb.Image(target)
	cmd = `sh -e -c "echo -n gzip > data"`
	st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", targetImg)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	compressedTarget := registry + "/buildkit/build/exporter:withcompressed"
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": compressedTarget,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	allCompressedTarget := registry + "/buildkit/build/exporter:withallcompressed"
	_, err = c.Solve(context.TODO(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              allCompressedTarget,
					"push":              "true",
					"compression":       "gzip",
					"force-compression": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	if cdAddress == "" {
		t.Skip("rest of test requires containerd worker")
	}

	err = client.ImageService().Delete(ctx, target, images.SynchronousDelete())
	require.NoError(t, err)
	err = client.ImageService().Delete(ctx, compressedTarget, images.SynchronousDelete())
	require.NoError(t, err)
	err = client.ImageService().Delete(ctx, allCompressedTarget, images.SynchronousDelete())
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)

	// check if the new layer is compressed with compression option
	img, err := client.Pull(ctx, compressedTarget)
	require.NoError(t, err)

	dt, err := content.ReadBlob(ctx, img.ContentStore(), img.Target())
	require.NoError(t, err)

	mfst := struct {
		MediaType string `json:"mediaType,omitempty"`
		ocispecs.Manifest
	}{}

	err = json.Unmarshal(dt, &mfst)
	require.NoError(t, err)
	require.Equal(t, 2, len(mfst.Layers))
	require.Equal(t, images.MediaTypeDockerSchema2Layer, mfst.Layers[0].MediaType)
	require.Equal(t, images.MediaTypeDockerSchema2LayerGzip, mfst.Layers[1].MediaType)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[0].Digest})
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	item, ok := m["data"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("uncompressed"), item.Data)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[1].Digest})
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, true)
	require.NoError(t, err)

	item, ok = m["data"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("gzip"), item.Data)

	err = client.ImageService().Delete(ctx, compressedTarget, images.SynchronousDelete())
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)

	// check if all layers are compressed with force-compressoin option
	img, err = client.Pull(ctx, allCompressedTarget)
	require.NoError(t, err)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), img.Target())
	require.NoError(t, err)

	mfst = struct {
		MediaType string `json:"mediaType,omitempty"`
		ocispecs.Manifest
	}{}

	err = json.Unmarshal(dt, &mfst)
	require.NoError(t, err)
	require.Equal(t, 2, len(mfst.Layers))
	require.Equal(t, images.MediaTypeDockerSchema2LayerGzip, mfst.Layers[0].MediaType)
	require.Equal(t, images.MediaTypeDockerSchema2LayerGzip, mfst.Layers[1].MediaType)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[0].Digest})
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, true)
	require.NoError(t, err)

	item, ok = m["data"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("uncompressed"), item.Data)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[1].Digest})
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, true)
	require.NoError(t, err)

	item, ok = m["data"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("gzip"), item.Data)
}

func testBuildExportZstd(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	cmd := `sh -e -c "echo -n zstd > data"`

	st := llb.Scratch()
	st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
				Attrs: map[string]string{
					"compression": "zstd",
				},
			},
		},
		// compression option should work even with inline cache exports
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]
	require.Equal(t, ocispecs.MediaTypeImageLayer+"+zstd", lastLayer.MediaType)

	zstdLayerDigest := lastLayer.Digest.Hex()
	require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd}, m[ocispecs.ImageBlobsDir+"/sha256/"+zstdLayerDigest].Data[:4])

	// repeat without oci mediatype
	outW, err = os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
				Attrs: map[string]string{
					"compression":    "zstd",
					"oci-mediatypes": "false",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(out)
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	lastLayer = mfst.Layers[len(mfst.Layers)-1]
	require.Equal(t, images.MediaTypeDockerSchema2Layer+".zstd", lastLayer.MediaType)

	require.Equal(t, lastLayer.Digest.Hex(), zstdLayerDigest)
}

func testPullZstdImage(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	for _, ociMediaTypes := range []bool{true, false} {
		t.Run(t.Name()+fmt.Sprintf("/ociMediaTypes=%t", ociMediaTypes), func(t *testing.T) {
			c, err := New(sb.Context(), sb.Address())
			require.NoError(t, err)
			defer c.Close()

			busybox := llb.Image("busybox:latest")
			cmd := `sh -e -c "echo -n zstd > data"`

			st := llb.Scratch()
			st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)

			def, err := st.Marshal(sb.Context())
			require.NoError(t, err)

			registry, err := sb.NewRegistry()
			if errors.Is(err, integration.ErrRequirements) {
				t.Skip(err.Error())
			}
			require.NoError(t, err)

			target := registry + "/buildkit/build/exporter:zstd"

			_, err = c.Solve(sb.Context(), def, SolveOpt{
				Exports: []ExportEntry{
					{
						Type: ExporterImage,
						Attrs: map[string]string{
							"name":           target,
							"push":           "true",
							"compression":    "zstd",
							"oci-mediatypes": strconv.FormatBool(ociMediaTypes),
						},
					},
				},
			}, nil)
			require.NoError(t, err)

			ensurePruneAll(t, c, sb)

			st = llb.Image(target).File(llb.Copy(llb.Image(target), "/data", "/zdata"))
			def, err = st.Marshal(sb.Context())
			require.NoError(t, err)

			destDir := t.TempDir()

			out := filepath.Join(destDir, "out.tar")
			outW, err := os.Create(out)
			require.NoError(t, err)

			_, err = c.Solve(sb.Context(), def, SolveOpt{
				Exports: []ExportEntry{
					{
						Type:   ExporterOCI,
						Output: fixedWriteCloser(outW),
						Attrs: map[string]string{
							"oci-mediatypes": strconv.FormatBool(ociMediaTypes),
						},
					},
				},
			}, nil)
			require.NoError(t, err)

			dt, err := os.ReadFile(out)
			require.NoError(t, err)

			m, err := testutil.ReadTarToMap(dt, false)
			require.NoError(t, err)

			var index ocispecs.Index
			err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
			require.NoError(t, err)

			var mfst ocispecs.Manifest
			err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
			require.NoError(t, err)

			firstLayer := mfst.Layers[0]
			if ociMediaTypes {
				require.Equal(t, ocispecs.MediaTypeImageLayer+"+zstd", firstLayer.MediaType)
			} else {
				require.Equal(t, images.MediaTypeDockerSchema2Layer+".zstd", firstLayer.MediaType)
			}

			zstdLayerDigest := firstLayer.Digest.Hex()
			require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd}, m[ocispecs.ImageBlobsDir+"/sha256/"+zstdLayerDigest].Data[:4])
		})
	}
}

func testBuildPushAndValidate(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -e -c "mkdir -p foo/sub; echo -n first > foo/sub/bar; chmod 0741 foo;"`)
	run(`true`) // this doesn't create a layer
	run(`sh -c "echo -n second > foo/sub/baz"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/testpush:latest"

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// test existence of the image with next build
	firstBuild := llb.Image(target)

	def, err = firstBuild.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo/sub/bar"))
	require.NoError(t, err)
	require.Equal(t, dt, []byte("first"))

	dt, err = os.ReadFile(filepath.Join(destDir, "foo/sub/baz"))
	require.NoError(t, err)
	require.Equal(t, dt, []byte("second"))

	fi, err := os.Stat(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, 0741, int(fi.Mode()&0777))

	checkAllReleasable(t, c, sb, false)

	// examine contents of exported tars (requires containerd)
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Skip("rest of test requires containerd worker")
	}

	// TODO: make public pull helper function so this can be checked for standalone as well

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

	// check image in containerd
	_, err = client.ImageService().Get(ctx, target)
	require.NoError(t, err)

	// deleting image should release all content
	err = client.ImageService().Delete(ctx, target, images.SynchronousDelete())
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)

	img, err := client.Pull(ctx, target)
	require.NoError(t, err)

	desc, err := img.Config(ctx)
	require.NoError(t, err)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), desc)
	require.NoError(t, err)

	var ociimg ocispecs.Image
	err = json.Unmarshal(dt, &ociimg)
	require.NoError(t, err)

	require.NotEqual(t, "", ociimg.OS)
	require.NotEqual(t, "", ociimg.Architecture)
	require.NotEqual(t, "", ociimg.Config.WorkingDir)
	require.Equal(t, "layers", ociimg.RootFS.Type)
	require.Equal(t, 3, len(ociimg.RootFS.DiffIDs))
	require.NotNil(t, ociimg.Created)
	require.Less(t, time.Since(*ociimg.Created), 2*time.Minute)
	require.Condition(t, func() bool {
		for _, env := range ociimg.Config.Env {
			if strings.HasPrefix(env, "PATH=") {
				return true
			}
		}
		return false
	})

	require.Equal(t, 3, len(ociimg.History))
	require.Contains(t, ociimg.History[0].CreatedBy, "foo/sub/bar")
	require.Contains(t, ociimg.History[1].CreatedBy, "true")
	require.Contains(t, ociimg.History[2].CreatedBy, "foo/sub/baz")
	require.False(t, ociimg.History[0].EmptyLayer)
	require.False(t, ociimg.History[1].EmptyLayer)
	require.False(t, ociimg.History[2].EmptyLayer)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), img.Target())
	require.NoError(t, err)

	mfst := struct {
		MediaType string `json:"mediaType,omitempty"`
		ocispecs.Manifest
	}{}

	err = json.Unmarshal(dt, &mfst)
	require.NoError(t, err)

	require.Equal(t, images.MediaTypeDockerSchema2Manifest, mfst.MediaType)
	require.Equal(t, 3, len(mfst.Layers))
	require.Equal(t, images.MediaTypeDockerSchema2LayerGzip, mfst.Layers[0].MediaType)
	require.Equal(t, images.MediaTypeDockerSchema2LayerGzip, mfst.Layers[1].MediaType)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[0].Digest})
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, true)
	require.NoError(t, err)

	item, ok := m["foo/"]
	require.True(t, ok)
	require.Equal(t, tar.TypeDir, int32(item.Header.Typeflag))
	require.Equal(t, 0741, int(item.Header.Mode&0777))

	item, ok = m["foo/sub/"]
	require.True(t, ok)
	require.Equal(t, tar.TypeDir, int32(item.Header.Typeflag))

	item, ok = m["foo/sub/bar"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("first"), item.Data)

	_, ok = m["foo/sub/baz"]
	require.False(t, ok)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), ocispecs.Descriptor{Digest: mfst.Layers[2].Digest})
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, true)
	require.NoError(t, err)

	item, ok = m["foo/sub/baz"]
	require.True(t, ok)
	require.Equal(t, tar.TypeReg, int32(item.Header.Typeflag))
	require.Equal(t, []byte("second"), item.Data)

	item, ok = m["foo/"]
	require.True(t, ok)
	require.Equal(t, tar.TypeDir, int32(item.Header.Typeflag))
	require.Equal(t, 0741, int(item.Header.Mode&0777))

	item, ok = m["foo/sub/"]
	require.True(t, ok)
	require.Equal(t, tar.TypeDir, int32(item.Header.Typeflag))

	_, ok = m["foo/sub/bar"]
	require.False(t, ok)
}

func testStargzLazyRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheBackendRegistry,
		workers.FeatureOCIExporter,
	)
	requiresLinux(t)
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" || sb.Snapshotter() != "stargz" {
		t.Skip("test requires containerd worker with stargz snapshotter")
	}

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	var (
		imageService = client.ImageService()
		contentStore = client.ContentStore()
		ctx          = namespaces.WithNamespace(sb.Context(), "buildkit")
	)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	destDir := t.TempDir()

	// Prepare stargz registry cache
	orgImage := "docker.io/library/alpine:latest"
	sgzCache := registry + "/stargz/alpinecache:" + identity.NewID()
	baseDef := llb.Image(orgImage).Run(llb.Args([]string{"/bin/touch", "/foo"}))
	def, err := baseDef.Marshal(sb.Context())
	require.NoError(t, err)
	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref":               sgzCache,
					"compression":       "estargz",
					"oci-mediatypes":    "true",
					"force-compression": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// clear all local state out
	ensurePruneAll(t, c, sb)
	workers.CheckFeatureCompat(t, sb, workers.FeatureCacheImport, workers.FeatureDirectPush)

	// stargz layers should be lazy even for executing something on them
	def, err = baseDef.
		Run(llb.Args([]string{"sh", "-c", "cat /dev/urandom | head -c 100 | sha256sum > unique"})).
		Marshal(sb.Context())
	require.NoError(t, err)
	target := registry + "/buildkit/testlazyimage:" + identity.NewID()
	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"store":                                  "true",
					"oci-mediatypes":                         "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": sgzCache,
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref":            sgzCache,
					"compression":    "estargz",
					"oci-mediatypes": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	unique, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst, "/unique")
	require.NoError(t, err)

	img, err := imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err := images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	// Check if image layers are lazy.
	// The topmost(last) layer created by `Run` isn't lazy so we skip the check for the layer.
	var sgzLayers []ocispecs.Descriptor
	for i, layer := range manifest.Layers[:len(manifest.Layers)-1] {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v on layer %+v (%d)", err, layer, i)
		sgzLayers = append(sgzLayers, layer)
	}
	require.NotEqual(t, 0, len(sgzLayers), "no layer can be used for checking lazypull")

	// The topmost(last) layer created by `Run` shouldn't be lazy
	_, err = contentStore.Info(ctx, manifest.Layers[len(manifest.Layers)-1].Digest)
	require.NoError(t, err)

	// Run build again and check if cache is reused
	resp, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"store":                                  "true",
					"oci-mediatypes":                         "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": sgzCache,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst2, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	unique2, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst2, "/unique")
	require.NoError(t, err)

	require.Equal(t, dgst, dgst2)
	require.Equal(t, unique, unique2)

	// clear all local state out
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// stargz layers should be exportable
	out = filepath.Join(destDir, "out2.tar")
	outW, err = os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": sgzCache,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// Check if image layers are un-lazied
	for _, layer := range sgzLayers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.NoError(t, err)
	}

	ensurePruneAll(t, c, sb)
}

func testStargzLazyInlineCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendInline,
		workers.FeatureCacheBackendRegistry,
	)
	requiresLinux(t)
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" || sb.Snapshotter() != "stargz" {
		t.Skip("test requires containerd worker with stargz snapshotter")
	}

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	var (
		imageService = client.ImageService()
		contentStore = client.ContentStore()
		ctx          = namespaces.WithNamespace(sb.Context(), "buildkit")
	)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// Prepare stargz inline cache
	orgImage := "docker.io/library/alpine:latest"
	sgzImage := registry + "/stargz/alpine:" + identity.NewID()
	baseDef := llb.Image(orgImage).Run(llb.Args([]string{"/bin/touch", "/foo"}))
	def, err := baseDef.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              sgzImage,
					"push":              "true",
					"compression":       "estargz",
					"oci-mediatypes":    "true",
					"force-compression": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
	}, nil)
	require.NoError(t, err)

	// clear all local state out
	err = imageService.Delete(ctx, sgzImage, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// stargz layers should be lazy even for executing something on them
	def, err = baseDef.
		Run(llb.Args([]string{"/bin/touch", "/bar"})).
		Marshal(sb.Context())
	require.NoError(t, err)
	target := registry + "/buildkit/testlazyimage:" + identity.NewID()
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"oci-mediatypes":                         "true",
					"compression":                            "estargz",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": sgzImage,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	img, err := imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err := images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	// Check if image layers are lazy.
	// The topmost(last) layer created by `Run` isn't lazy so we skip the check for the layer.
	var sgzLayers []ocispecs.Descriptor
	for i, layer := range manifest.Layers[:len(manifest.Layers)-1] {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v on layer %+v (%d)", err, layer, i)
		sgzLayers = append(sgzLayers, layer)
	}
	require.NotEqual(t, 0, len(sgzLayers), "no layer can be used for checking lazypull")

	// The topmost(last) layer created by `Run` shouldn't be lazy
	_, err = contentStore.Info(ctx, manifest.Layers[len(manifest.Layers)-1].Digest)
	require.NoError(t, err)

	// clear all local state out
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// stargz layers should be exportable
	destDir := t.TempDir()
	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": sgzImage,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// Check if image layers are un-lazied
	for _, layer := range sgzLayers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.NoError(t, err)
	}

	ensurePruneAll(t, c, sb)
}

func testStargzLazyPull(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)

	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" || sb.Snapshotter() != "stargz" {
		t.Skip("test requires containerd worker with stargz snapshotter")
	}

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	var (
		imageService = client.ImageService()
		contentStore = client.ContentStore()
		ctx          = namespaces.WithNamespace(sb.Context(), "buildkit")
	)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// Prepare stargz image
	orgImage := "docker.io/library/alpine:latest"
	sgzImage := registry + "/stargz/alpine:" + identity.NewID()
	def, err := llb.Image(orgImage).Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              sgzImage,
					"push":              "true",
					"compression":       "estargz",
					"oci-mediatypes":    "true",
					"force-compression": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// clear all local state out
	err = imageService.Delete(ctx, sgzImage, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// stargz layers should be lazy even for executing something on them
	def, err = llb.Image(sgzImage).
		Run(llb.Args([]string{"sh", "-c", "cat /dev/urandom | head -c 100 | sha256sum > unique"})).
		Marshal(sb.Context())
	require.NoError(t, err)
	target := registry + "/buildkit/testlazyimage:" + identity.NewID()
	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"oci-mediatypes":                         "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	unique, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst, "/unique")
	require.NoError(t, err)

	img, err := imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err := images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	// Check if image layers are lazy.
	// The topmost(last) layer created by `Run` isn't lazy so we skip the check for the layer.
	var sgzLayers []ocispecs.Descriptor
	for _, layer := range manifest.Layers[:len(manifest.Layers)-1] {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
		sgzLayers = append(sgzLayers, layer)
	}
	require.NotEqual(t, 0, len(sgzLayers), "no layer can be used for checking lazypull")

	// The topmost(last) layer created by `Run` shouldn't be lazy
	_, err = contentStore.Info(ctx, manifest.Layers[len(manifest.Layers)-1].Digest)
	require.NoError(t, err)

	// Run build again and check if cache is reused
	resp, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"store":                                  "true",
					"oci-mediatypes":                         "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst2, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	unique2, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst2, "/unique")
	require.NoError(t, err)

	require.Equal(t, dgst, dgst2)
	require.Equal(t, unique, unique2)

	// clear all local state out
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// stargz layers should be exportable
	destDir := t.TempDir()
	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	// Check if image layers are un-lazied
	for _, layer := range sgzLayers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.NoError(t, err)
	}

	ensurePruneAll(t, c, sb)
}

func testLazyImagePush(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Skip("test requires containerd worker")
	}

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// push the busybox image to the mutable registry
	sourceImage := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	def, err := llb.Image(sourceImage).Marshal(sb.Context())
	require.NoError(t, err)

	targetNoTag := registry + "/buildkit/testlazyimage:"
	target := targetNoTag + "latest"
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	imageService := client.ImageService()
	contentStore := client.ContentStore()

	img, err := imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err := images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.NoError(t, err)
	}

	// clear all local state out
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// retag the image we just pushed with no actual changes, which
	// should not result in the image getting un-lazied
	def, err = llb.Image(target).Marshal(sb.Context())
	require.NoError(t, err)

	target2 := targetNoTag + "newtag"
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target2,
					"push":                                   "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	img, err = imageService.Get(ctx, target2)
	require.NoError(t, err)

	manifest, err = images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	// clear all local state out again
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	// try a cross-repo push to same registry, which should still result in the
	// image remaining lazy
	target3 := registry + "/buildkit/testlazycrossrepo:latest"
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target3,
					"push":                                   "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	img, err = imageService.Get(ctx, target3)
	require.NoError(t, err)

	manifest, err = images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	// check that a subsequent build can use the previously lazy image in an exec
	cmdStr := integration.UnixOrWindows("true", "cmd /C exit 0")
	def, err = llb.Image(target2).Run(llb.Args([]string{cmdStr})).Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testZstdLocalCacheExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureCacheExport, workers.FeatureCacheBackendLocal)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	cmd := `sh -e -c "echo -n zstd > data"`

	st := llb.Scratch()
	st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()
	destOutDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destOutDir,
			},
		},
		// compression option should work even with inline cache exports
		CacheExports: []CacheOptionsEntry{
			{
				Type: "local",
				Attrs: map[string]string{
					"dest":           destDir,
					"image-manifest": "false",
					"compression":    "zstd",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	var index ocispecs.Index
	dt, err := os.ReadFile(filepath.Join(destDir, ocispecs.ImageIndexFile))
	require.NoError(t, err)
	err = json.Unmarshal(dt, &index)
	require.NoError(t, err)

	var layerIndex ocispecs.Index
	dt, err = os.ReadFile(filepath.Join(destDir, ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()))
	require.NoError(t, err)
	err = json.Unmarshal(dt, &layerIndex)
	require.NoError(t, err)

	lastLayer := layerIndex.Manifests[len(layerIndex.Manifests)-2]
	require.Equal(t, ocispecs.MediaTypeImageLayer+"+zstd", lastLayer.MediaType)

	zstdLayerDigest := lastLayer.Digest.Hex()
	dt, err = os.ReadFile(filepath.Join(destDir, ocispecs.ImageBlobsDir+"/sha256/"+zstdLayerDigest))
	require.NoError(t, err)
	require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd}, dt[:4])
}

func testCacheExportIgnoreError(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureCacheExport)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	cmd := integration.UnixOrWindows(
		`sh -e -c "echo -n ignore-error > data"`,
		"cmd /C echo ignore-error> data",
	)

	st := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))
	st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	tests := map[string]struct {
		Exports        []ExportEntry
		CacheExports   []CacheOptionsEntry
		expectedErrors []string
	}{
		"local-ignore-error": {
			Exports: []ExportEntry{
				{
					Type:      ExporterLocal,
					OutputDir: t.TempDir(),
				},
			},
			CacheExports: []CacheOptionsEntry{
				{
					Type: "local",
					Attrs: map[string]string{
						"dest": "éèç",
					},
				},
			},
			expectedErrors: []string{"failed to solve", "contains value with non-printable ASCII characters"},
		},
		"registry-ignore-error": {
			Exports: []ExportEntry{
				{
					Type: ExporterImage,
					Attrs: map[string]string{
						"name": "test-registry-ignore-error",
						"push": "false",
					},
				},
			},
			CacheExports: []CacheOptionsEntry{
				{
					Type: "registry",
					Attrs: map[string]string{
						"ref": "fake-url:5000/myrepo:buildcache",
					},
				},
			},
			expectedErrors: []string{"failed to solve", "dial tcp: lookup fake-url", "no such host"},
		},
		"s3-ignore-error": {
			Exports: []ExportEntry{
				{
					Type:      ExporterLocal,
					OutputDir: t.TempDir(),
				},
			},
			CacheExports: []CacheOptionsEntry{
				{
					Type: "s3",
					Attrs: map[string]string{
						"endpoint_url":      "http://fake-url:9000",
						"bucket":            "my-bucket",
						"region":            "us-east-1",
						"access_key_id":     "minioadmin",
						"secret_access_key": "minioadmin",
						"use_path_style":    "true",
					},
				},
			},
			expectedErrors: []string{"failed to solve", "dial tcp: lookup fake-url", "no such host"},
		},
	}
	ignoreErrorValues := []bool{true, false}
	for _, ignoreError := range ignoreErrorValues {
		ignoreErrStr := strconv.FormatBool(ignoreError)
		for n, test := range tests {
			require.Equal(t, 1, len(test.Exports))
			require.Equal(t, 1, len(test.CacheExports))
			require.NotEmpty(t, test.CacheExports[0].Attrs)
			test.CacheExports[0].Attrs["ignore-error"] = ignoreErrStr
			testName := fmt.Sprintf("%s-%s", n, ignoreErrStr)
			t.Run(testName, func(t *testing.T) {
				switch n {
				case "local-ignore-error":
					workers.CheckFeatureCompat(t, sb, workers.FeatureCacheBackendLocal)
				case "registry-ignore-error":
					workers.CheckFeatureCompat(t, sb, workers.FeatureCacheBackendRegistry)
				case "s3-ignore-error":
					workers.CheckFeatureCompat(t, sb, workers.FeatureCacheBackendS3)
				}
				_, err = c.Solve(sb.Context(), def, SolveOpt{
					Exports:      test.Exports,
					CacheExports: test.CacheExports,
				}, nil)
				if ignoreError {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					for _, errStr := range test.expectedErrors {
						require.Contains(t, err.Error(), errStr)
					}
				}
			})
		}
	}
}

func testUncompressedLocalCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendLocal,
	)
	dir := t.TempDir()
	im := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"src": dir,
		},
	}
	ex := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"dest":              dir,
			"compression":       "uncompressed",
			"force-compression": "true",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testUncompressedRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	im := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}
	ex := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref":               target,
			"compression":       "uncompressed",
			"force-compression": "true",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testZstdLocalCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")

	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendLocal,
	)
	dir := t.TempDir()
	im := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"src": dir,
		},
	}
	ex := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"dest":              dir,
			"compression":       "zstd",
			"force-compression": "true",
			"oci-mediatypes":    "true", // containerd applier supports only zstd with oci-mediatype.
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testImageManifestRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	im := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}
	ex := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref":            target,
			"oci-mediatypes": "true",
			"mode":           "max",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testZstdRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")

	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	im := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}
	ex := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref":               target,
			"compression":       "zstd",
			"force-compression": "true",
			"image-manifest":    "false",
			"oci-mediatypes":    "true", // containerd applier supports only zstd with oci-mediatype.
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testBasicCacheImportExport(t *testing.T, sb integration.Sandbox, cacheOptionsEntryImport, cacheOptionsEntryExport []CacheOptionsEntry) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(integration.UnixOrWindows(
		`sh -c "echo -n foobar > const"`,
		`cmd /C echo foobar > const`,
	))
	run(integration.UnixOrWindows(
		`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`,
		`cmd /C echo %RANDOM% > unique`,
	))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheExports: cacheOptionsEntryExport,
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "const"))
	require.NoError(t, err)
	newLine := integration.UnixOrWindows("", " \r\n")
	require.Equal(t, "foobar"+newLine, string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)

	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: cacheOptionsEntryImport,
	}, nil)
	require.NoError(t, err)

	dt2, err := os.ReadFile(filepath.Join(destDir, "const"))
	require.NoError(t, err)
	require.Equal(t, "foobar"+newLine, string(dt2))

	dt2, err = os.ReadFile(filepath.Join(destDir, "unique"))
	require.NoError(t, err)
	require.Equal(t, string(dt), string(dt2))
}

func testBasicRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	o := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{o}, []CacheOptionsEntry{o})
}

func testMultipleRegistryCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	o := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}
	o2 := CacheOptionsEntry{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target + "notexist",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{o, o2}, []CacheOptionsEntry{o})
}

func testBasicLocalCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendLocal,
	)
	dir := t.TempDir()
	im := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"src": dir,
		},
	}
	ex := CacheOptionsEntry{
		Type: "local",
		Attrs: map[string]string{
			"dest": dir,
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testBasicS3CacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendS3,
	)

	opts := helpers.MinioOpts{
		Region:          "us-east-1",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
	}

	s3Addr, s3Bucket, cleanup, err := helpers.NewMinioServer(t, sb, opts)
	require.NoError(t, err)
	defer cleanup()

	im := CacheOptionsEntry{
		Type: "s3",
		Attrs: map[string]string{
			"region":            opts.Region,
			"access_key_id":     opts.AccessKeyID,
			"secret_access_key": opts.SecretAccessKey,
			"bucket":            s3Bucket,
			"endpoint_url":      s3Addr,
			"use_path_style":    "true",
		},
	}
	ex := CacheOptionsEntry{
		Type: "s3",
		Attrs: map[string]string{
			"region":            opts.Region,
			"access_key_id":     opts.AccessKeyID,
			"secret_access_key": opts.SecretAccessKey,
			"bucket":            s3Bucket,
			"endpoint_url":      s3Addr,
			"use_path_style":    "true",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testBasicAzblobCacheImportExport(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendAzblob,
	)

	opts := helpers.AzuriteOpts{
		AccountName: "azblobcacheaccount",
		AccountKey:  base64.StdEncoding.EncodeToString([]byte("azblobcacheaccountkey")),
	}

	azAddr, cleanup, err := helpers.NewAzuriteServer(t, sb, opts)
	require.NoError(t, err)
	defer cleanup()

	im := CacheOptionsEntry{
		Type: "azblob",
		Attrs: map[string]string{
			"account_url":       azAddr,
			"account_name":      opts.AccountName,
			"secret_access_key": opts.AccountKey,
			"container":         "cachecontainer",
		},
	}
	ex := CacheOptionsEntry{
		Type: "azblob",
		Attrs: map[string]string{
			"account_url":       azAddr,
			"account_name":      opts.AccountName,
			"secret_access_key": opts.AccountKey,
			"container":         "cachecontainer",
		},
	}
	testBasicCacheImportExport(t, sb, []CacheOptionsEntry{im}, []CacheOptionsEntry{ex})
}

func testBasicInlineCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureDirectPush,
		workers.FeatureCacheExport,
		workers.FeatureCacheBackendInline,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(integration.UnixOrWindows(
		`sh -c "echo -n foobar > const"`,
		`cmd /C echo foobar > const`,
	))
	run(integration.UnixOrWindows(
		`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`,
		`cmd /C echo %RANDOM% > unique`,
	))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	target := registry + "/buildkit/testexportinline:latest"

	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	unique, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst, "/unique")
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)
	workers.CheckFeatureCompat(t, sb, workers.FeatureCacheImport, workers.FeatureCacheBackendRegistry)

	resp, err = c.Solve(sb.Context(), def, SolveOpt{
		// specifying inline cache exporter is needed for reproducing containerimage.digest
		// (not needed for reproducing rootfs/unique)
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": target,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst2, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	require.Equal(t, dgst, dgst2)

	ensurePruneAll(t, c, sb)

	// Export the cache again with compression
	resp, err = c.Solve(sb.Context(), def, SolveOpt{
		// specifying inline cache exporter is needed for reproducing containerimage.digest
		// (not needed for reproducing rootfs/unique)
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":              target,
					"push":              "true",
					"compression":       "uncompressed", // inline cache should work with compression
					"force-compression": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": target,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst2uncompress, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	// dgst2uncompress != dgst, because the compression type is different
	unique2uncompress, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst2uncompress, "/unique")
	require.NoError(t, err)
	require.Equal(t, unique, unique2uncompress)

	ensurePruneAll(t, c, sb)

	resp, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": target,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst3, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, true, ok)

	// dgst3 != dgst, because inline cache is not exported for dgst3
	unique3, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst3, "/unique")
	require.NoError(t, err)
	require.Equal(t, unique, unique3)
}

func testRegistryEmptyCacheExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheBackendRegistry,
	)

	for _, ociMediaTypes := range []bool{true, false} {
		for _, imageManifest := range []bool{true, false} {
			if imageManifest && !ociMediaTypes {
				// invalid configuration for remote cache
				continue
			}

			t.Run(t.Name()+fmt.Sprintf("/ociMediaTypes=%t/imageManifest=%t", ociMediaTypes, imageManifest), func(t *testing.T) {
				c, err := New(sb.Context(), sb.Address())
				require.NoError(t, err)
				defer c.Close()

				st := llb.Scratch()
				def, err := st.Marshal(sb.Context())
				require.NoError(t, err)

				registry, err := sb.NewRegistry()
				if errors.Is(err, integration.ErrRequirements) {
					t.Skip(err.Error())
				}
				require.NoError(t, err)

				cacheTarget := registry + "/buildkit/testregistryemptycache:latest"

				cacheOptionsEntry := CacheOptionsEntry{
					Type: "registry",
					Attrs: map[string]string{
						"ref":            cacheTarget,
						"image-manifest": strconv.FormatBool(imageManifest),
						"oci-mediatypes": strconv.FormatBool(ociMediaTypes),
					},
				}

				_, err = c.Solve(sb.Context(), def, SolveOpt{
					CacheExports: []CacheOptionsEntry{cacheOptionsEntry},
				}, nil)
				require.NoError(t, err)

				ctx := namespaces.WithNamespace(sb.Context(), "buildkit")
				cdAddress := sb.ContainerdAddress()
				var client *ctd.Client
				if cdAddress != "" {
					client, err = newContainerd(cdAddress)
					require.NoError(t, err)
					defer client.Close()

					_, err := client.Fetch(ctx, cacheTarget)
					require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
				}
			})
		}
	}
}

func testMultipleRecordsWithSameLayersCacheImportExport(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb,
		workers.FeatureCacheExport,
		workers.FeatureCacheImport,
		workers.FeatureCacheBackendRegistry,
	)
	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/testexport:latest"
	cacheOpts := []CacheOptionsEntry{{
		Type: "registry",
		Attrs: map[string]string{
			"ref": target,
		},
	}}

	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	base := llb.Image("busybox:latest")
	// layerA and layerB create identical layers with different LLB
	layerA := base.Run(llb.Args([]string{
		"sh", "-c",
		`echo $(( 1 + 2 )) > /result && touch -d "1970-01-01 00:00:00" /result`,
	})).Root()
	layerB := base.Run(llb.Args([]string{
		"sh", "-c",
		`echo $(( 2 + 1 )) > /result && touch -d "1970-01-01 00:00:00" /result`,
	})).Root()

	combined := llb.Merge([]llb.State{layerA, layerB})
	combinedDef, err := combined.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), combinedDef, SolveOpt{
		CacheExports: cacheOpts,
	}, nil)
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)

	singleDef, err := layerA.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), singleDef, SolveOpt{
		CacheImports: cacheOpts,
	}, nil)
	require.NoError(t, err)

	// Ensure that even though layerA and layerB were both loaded as possible results
	// and only was used, all the cache refs are released
	// More context: https://github.com/moby/buildkit/pull/3815
	ensurePruneAll(t, c, sb)
}

// moby/buildkit#3809
func testSnapshotWithMultipleBlobs(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// build two images with same layer but different compressed blobs

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	now := time.Now()

	st := llb.Scratch().File(llb.Copy(llb.Image("alpine"), "/", "/alpine/", llb.WithCreatedTime(now)))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	name1 := registry + "/multiblobtest1/image:latest"

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: "image",
				Attrs: map[string]string{
					"name":              name1,
					"push":              "true",
					"compression-level": "0",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)

	st = st.File(llb.Mkfile("test", 0600, []byte("test"))) // extra layer so we don't get a cache match based on image config rootfs only

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	name2 := registry + "/multiblobtest2/image:latest"

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: "image",
				Attrs: map[string]string{
					"name":              name2,
					"push":              "true",
					"compression-level": "9",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	ensurePruneAll(t, c, sb)

	// first build with first image
	destDir := t.TempDir()

	out1 := filepath.Join(destDir, "out1.tar")
	outW1, err := os.Create(out1)
	require.NoError(t, err)

	st = llb.Image(name1).File(llb.Mkfile("test", 0600, []byte("test1")))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI, // make sure to export so blobs need to be loaded
				Output: fixedWriteCloser(outW1),
			},
		},
	}, nil)
	require.NoError(t, err)

	// make sure second image does not cause any errors
	out2 := filepath.Join(destDir, "out2.tar")
	outW2, err := os.Create(out2)
	require.NoError(t, err)

	st = llb.Image(name2).File(llb.Mkfile("test", 0600, []byte("test2")))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "",
		},
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI, // make sure to export so blobs need to be loaded
				Output: fixedWriteCloser(outW2),
			},
		},
	}, nil)
	require.NoError(t, err)

	// extra validation that we did get different layer blobs
	dt, err := os.ReadFile(out1)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	dt, err = os.ReadFile(out2)
	require.NoError(t, err)

	m, err = testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &index)
	require.NoError(t, err)

	var mfst2 ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst2)
	require.NoError(t, err)

	require.NotEqual(t, mfst.Layers[0].Digest, mfst2.Layers[0].Digest)
}

func testExportLocalNoPlatformSplit(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureMultiPlatform)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	platformsToTest := []string{"linux/amd64", "linux/arm64"}
	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(platformsToTest)),
		}
		for i, platform := range platformsToTest {
			st := llb.Scratch().File(
				llb.Mkfile("hello-"+strings.ReplaceAll(platform, "/", "-"), 0600, []byte(platform)),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(platform, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       platform,
				Platform: platforms.MustParse(platform),
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	destDir := t.TempDir()
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
				Attrs: map[string]string{
					"platform-split": "false",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "hello-linux-amd64"))
	require.NoError(t, err)
	require.Equal(t, "linux/amd64", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "hello-linux-arm64"))
	require.NoError(t, err)
	require.Equal(t, "linux/arm64", string(dt))
}

func testExportLocalNoPlatformSplitOverwrite(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureMultiPlatform)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	platformsToTest := []string{"linux/amd64", "linux/arm64"}
	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(platformsToTest)),
		}
		for i, platform := range platformsToTest {
			st := llb.Scratch().File(
				llb.Mkfile("hello-linux", 0600, []byte(platform)),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(platform, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       platform,
				Platform: platforms.MustParse(platform),
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	destDir := t.TempDir()
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
				Attrs: map[string]string{
					"platform-split": "false",
				},
			},
		},
	}, "", frontend, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot overwrite hello-linux from")
	require.ErrorContains(t, err, "when split option is disabled")
}

func testExportLocalForcePlatformSplit(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter, workers.FeatureMultiPlatform)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.Scratch().File(
			llb.Mkfile("foo", 0600, []byte("hello")),
		)

		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}

		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}

	destDir := t.TempDir()
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
				Attrs: map[string]string{
					"platform-split": "true",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	fis, err := os.ReadDir(destDir)
	require.NoError(t, err)

	require.Len(t, fis, 1, "expected one files in the output directory")

	expPlatform := strings.ReplaceAll(platforms.FormatAll(platforms.DefaultSpec()), "/", "_")
	_, err = os.Stat(filepath.Join(destDir, expPlatform+"/"))
	require.NoError(t, err)

	fis, err = os.ReadDir(filepath.Join(destDir, expPlatform))
	require.NoError(t, err)

	require.Len(t, fis, 1, "expected one files in the output directory for platform "+expPlatform)

	dt, err := os.ReadFile(filepath.Join(destDir, expPlatform, "foo"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(dt))
}

func readFileInImage(ctx context.Context, t *testing.T, c *Client, ref, path string) ([]byte, error) {
	def, err := llb.Image(ref).Marshal(ctx)
	if err != nil {
		return nil, err
	}
	destDir := t.TempDir()

	_, err = c.Solve(ctx, def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(destDir, filepath.Clean(path)))
}

func testCachedMounts(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	scratch := func() llb.State {
		return integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))
	}

	busybox := llb.Image(imgName)
	// setup base for one of the cache sources
	cmdPrefix := integration.UnixOrWindows(
		`sh -c "echo -n`,
		`cmd /C "echo`,
	)
	st := busybox.Run(llb.Shlexf(`%s base > baz"`, cmdPrefix), llb.Dir("/wd"))
	base := st.AddMount("/wd", scratch())

	st = busybox.Run(llb.Shlexf(`%s first > foo"`, cmdPrefix), llb.Dir("/wd"))

	st.AddMount("/wd", scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))
	cmdStr := integration.UnixOrWindows(
		`sh -c "cat foo && echo -n second > /wd2/bar"`,
		`cmd /C type foo && echo second > C:\\wd2\\bar`,
	)
	st = st.Run(llb.Shlex(cmdStr), llb.Dir("/wd"))
	st.AddMount("/wd", scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))
	st.AddMount("/wd2", base, llb.AsPersistentCacheDir("mycache2", llb.CacheMountShared))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// repeat to make sure cache works
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// second build using cache directories
	cmdStr = integration.UnixOrWindows(
		`sh -c "cp /src0/foo . && cp /src1/bar . && cp /src1/baz ."`,
		`cmd /C copy C:\\src0\\foo . && copy C:\\src1\\bar . && copy C:\\src1\\baz .`,
	)
	st = busybox.Run(llb.Shlex(cmdStr), llb.Dir("/wd"))
	out := st.AddMount("/wd", scratch())
	st.AddMount("/src0", scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))
	st.AddMount("/src1", base, llb.AsPersistentCacheDir("mycache2", llb.CacheMountShared))

	destDir := t.TempDir()

	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	newLine := integration.UnixOrWindows("", " \r\n")
	dt, err := os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, "first"+newLine, string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "bar"))
	require.NoError(t, err)
	require.Equal(t, "second"+newLine, string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "baz"))
	require.NoError(t, err)
	require.Equal(t, "base"+newLine, string(dt))

	checkAllReleasable(t, c, sb, true)
}

func testSharedCacheMounts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := busybox.Run(llb.Shlex(`sh -e -c "touch one; while [[ ! -f two ]]; do ls -l; usleep 500000; done"`), llb.Dir("/wd"))
	st.AddMount("/wd", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))

	st2 := busybox.Run(llb.Shlex(`sh -e -c "touch two; while [[ ! -f one ]]; do ls -l; usleep 500000; done"`), llb.Dir("/wd"))
	st2.AddMount("/wd", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))

	out := busybox.Run(llb.Shlex("true"))
	out.AddMount("/m1", st.Root())
	out.AddMount("/m2", st2.Root())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

// #2334
func testSharedCacheMountsNoScratch(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := busybox.Run(llb.Shlex(`sh -e -c "touch one; while [[ ! -f two ]]; do ls -l; usleep 500000; done"`), llb.Dir("/wd"))
	st.AddMount("/wd", llb.Image("busybox:latest"), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))

	st2 := busybox.Run(llb.Shlex(`sh -e -c "touch two; while [[ ! -f one ]]; do ls -l; usleep 500000; done"`), llb.Dir("/wd"))
	st2.AddMount("/wd", llb.Image("busybox:latest"), llb.AsPersistentCacheDir("mycache1", llb.CacheMountShared))

	out := busybox.Run(llb.Shlex("true"))
	out.AddMount("/m1", st.Root())
	out.AddMount("/m2", st2.Root())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testLockedCacheMounts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := busybox.Run(llb.Shlex(`sh -e -c "touch one; if [[ -f two ]]; then exit 0; fi; for i in $(seq 10); do if [[ -f two ]]; then exit 1; fi; usleep 200000; done"`), llb.Dir("/wd"))
	st.AddMount("/wd", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))

	st2 := busybox.Run(llb.Shlex(`sh -e -c "touch two; if [[ -f one ]]; then exit 0; fi; for i in $(seq 10); do if [[ -f one ]]; then exit 1; fi; usleep 200000; done"`), llb.Dir("/wd"))
	st2.AddMount("/wd", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))

	out := busybox.Run(llb.Shlex("true"))
	out.AddMount("/m1", st.Root())
	out.AddMount("/m2", st2.Root())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testDuplicateCacheMount(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	scratch := func() llb.State {
		return integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))
	}

	cmdStr := integration.UnixOrWindows(
		`sh -e -c "[[ ! -f /m2/foo ]]; touch /m1/foo; [[ -f /m2/foo ]];"`,
		`cmd /C echo a > \\m1\\foo && dir \\m2\\foo`,
	)
	out := busybox.Run(llb.Shlex(cmdStr))
	out.AddMount("/m1", scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))
	out.AddMount("/m2", scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testRunCacheWithMounts(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)

	cmdStr := integration.UnixOrWindows(
		`sh -e -c "[[ -f /m1/sbin/apk ]]"`,
		`cmd /C if exist C:\\m1\\Windows\\System32\\whoami.exe (exit 0) else (exit 1)`,
	)
	out := busybox.Run(llb.Shlex(cmdStr))
	imgAlpine := integration.UnixOrWindows("alpine:latest", "nanoserver:plus")
	out.AddMount("/m1", llb.Image(imgAlpine), llb.Readonly)

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	cmdStr = integration.UnixOrWindows(
		`sh -e -c "[[ ! -f /m1/sbin/apk ]]"`,
		`cmd /C if exist C:\\m1\\Windows\\System32\\whoami.exe (exit 1)`,
	)
	out = busybox.Run(llb.Shlex(cmdStr))
	out.AddMount("/m1", llb.Image(imgName), llb.Readonly)

	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testCacheMountNoCache(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")

	out := busybox.Run(llb.Shlex(`sh -e -c "touch /m1/foo; touch /m2/bar"`))
	out.AddMount("/m1", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))
	out.AddMount("/m2", llb.Scratch(), llb.AsPersistentCacheDir("mycache2", llb.CacheMountLocked))

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	out = busybox.Run(llb.Shlex(`sh -e -c "[[ ! -f /m1/foo ]]; touch /m1/foo2;"`), llb.IgnoreCache)
	out.AddMount("/m1", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))

	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	out = busybox.Run(llb.Shlex(`sh -e -c "[[ -f /m1/foo2 ]]; [[ -f /m2/bar ]];"`))
	out.AddMount("/m1", llb.Scratch(), llb.AsPersistentCacheDir("mycache1", llb.CacheMountLocked))
	out.AddMount("/m2", llb.Scratch(), llb.AsPersistentCacheDir("mycache2", llb.CacheMountLocked))

	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testCopyFromEmptyImage(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// On Windows, the error messages are different for the Scratch image
	// (which is coming from the OS). While the one for the no-layers image
	// is being returned by Buildkit (in unix style).
	winErrMsgs := []string{
		"foo: The system cannot find the file specified", // for llb.Scratch()
		"/foo: no such file or directory",                // for tonistiigi/test:nolayers
	}

	for i, image := range []llb.State{llb.Scratch(), llb.Image("tonistiigi/test:nolayers")} {
		st := llb.Scratch().File(llb.Copy(image, "/", "/"))
		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)

		_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
		require.NoError(t, err)

		st = llb.Scratch().File(llb.Copy(image, "/foo", "/"))
		def, err = st.Marshal(sb.Context())
		require.NoError(t, err)

		_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
		require.Error(t, err)
		errMsg := integration.UnixOrWindows(
			"/foo: no such file or directory",
			winErrMsgs[i],
		)
		require.Contains(t, err.Error(), errMsg)

		imgName := integration.UnixOrWindows(
			"busybox:latest",
			"mcr.microsoft.com/windows/nanoserver:ltsc2022",
		)
		busybox := llb.Image(imgName)

		cmdStr := integration.UnixOrWindows(
			`sh -e -c '[ $(ls /scratch | wc -l) = '0' ]'`,
			`cmd /C dir \scratch`,
		)
		out := busybox.Run(llb.Shlex(cmdStr))
		out.AddMount("/scratch", image, llb.Readonly)

		def, err = out.Marshal(sb.Context())
		require.NoError(t, err)

		_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
		require.NoError(t, err)
	}
}

// containerd/containerd#2119
func testDuplicateWhiteouts(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -e -c "mkdir -p d0 d1; echo -n first > d1/bar;"`)
	run(`sh -c "rm -rf d0 d1"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]

	layer, ok := m[ocispecs.ImageBlobsDir+"/sha256/"+lastLayer.Digest.Hex()]
	require.True(t, ok)

	m, err = testutil.ReadTarToMap(layer.Data, true)
	require.NoError(t, err)

	_, ok = m[".wh.d0"]
	require.True(t, ok)

	_, ok = m[".wh.d1"]
	require.True(t, ok)

	// check for a bug that added whiteout for subfile
	_, ok = m["d1/.wh.bar"]
	require.True(t, !ok)
}

// #276
func testWhiteoutParentDir(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "mkdir -p foo; echo -n first > foo/bar;"`)
	run(`rm foo/bar`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]

	layer, ok := m[ocispecs.ImageBlobsDir+"/sha256/"+lastLayer.Digest.Hex()]
	require.True(t, ok)

	m, err = testutil.ReadTarToMap(layer.Data, true)
	require.NoError(t, err)

	_, ok = m["foo/.wh.bar"]
	require.True(t, ok)

	_, ok = m["foo/"]
	require.True(t, ok)
}

// #2490
func testMoveParentDir(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "mkdir -p foo; echo -n first > foo/bar;"`)
	run(`mv foo foo2`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index)
	require.NoError(t, err)

	var mfst ocispecs.Manifest
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]

	layer, ok := m[ocispecs.ImageBlobsDir+"/sha256/"+lastLayer.Digest.Hex()]
	require.True(t, ok)

	m, err = testutil.ReadTarToMap(layer.Data, true)
	require.NoError(t, err)

	_, ok = m[".wh.foo"]
	require.True(t, ok)

	_, ok = m["foo2/"]
	require.True(t, ok)

	_, ok = m["foo2/bar"]
	require.True(t, ok)
}

// #319
func testMountWithNoSource(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	busybox := llb.Image(imgName)
	st := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))

	var nilState llb.State

	// This should never actually be run, but we want to succeed
	// if it was, because we expect an error below, or a daemon
	// panic if the issue has regressed.
	cmdArgs := integration.UnixOrWindows(
		[]string{"/bin/true"},
		[]string{"cmd", "/C", "exit 0"},
	)
	run := busybox.Run(
		llb.Args(cmdArgs),
		llb.AddMount("/nil", nilState, llb.SourcePath("/"), llb.Readonly))

	st = run.AddMount("/mnt", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)
}

// #324
func testReadonlyRootFS(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("docker.io/library/busybox:latest")
	st := llb.Scratch()

	// The path /foo should be unwriteable.
	run := busybox.Run(
		llb.ReadonlyRootFS(),
		llb.Args([]string{"/bin/touch", "/foo"}))
	st = run.AddMount("/mnt", st)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	// Would prefer to detect more specifically "Read-only file
	// system" but that isn't exposed here (it is on the stdio
	// which we don't see).
	require.Contains(t, err.Error(), "process \"/bin/touch /foo\" did not complete successfully")

	checkAllReleasable(t, c, sb, true)
}

func testSourceMap(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	sm1 := llb.NewSourceMap(nil, "foo", "", []byte("data1"))
	sm2 := llb.NewSourceMap(nil, "bar", "", []byte("data2"))

	st := llb.Scratch().Run(
		llb.Shlex("not-exist"),
		sm1.Location([]*pb.Range{{Start: &pb.Position{Line: 7}}}),
		sm2.Location([]*pb.Range{{Start: &pb.Position{Line: 8}}}),
		sm1.Location([]*pb.Range{{Start: &pb.Position{Line: 9}}}),
	)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)

	srcs := errdefs.Sources(err)
	require.Equal(t, 3, len(srcs))

	// Source errors are wrapped in the order provided as llb.ConstraintOpts, so
	// when they are unwrapped, the first unwrapped error is the last location
	// provided.
	require.Equal(t, "foo", srcs[0].Info.Filename)
	require.Equal(t, []byte("data1"), srcs[0].Info.Data)
	require.Nil(t, srcs[0].Info.Definition)

	require.Equal(t, 1, len(srcs[0].Ranges))
	require.Equal(t, int32(9), srcs[0].Ranges[0].Start.Line)
	require.Equal(t, int32(0), srcs[0].Ranges[0].Start.Character)

	require.Equal(t, "bar", srcs[1].Info.Filename)
	require.Equal(t, []byte("data2"), srcs[1].Info.Data)
	require.Nil(t, srcs[1].Info.Definition)

	require.Equal(t, 1, len(srcs[1].Ranges))
	require.Equal(t, int32(8), srcs[1].Ranges[0].Start.Line)
	require.Equal(t, int32(0), srcs[1].Ranges[0].Start.Character)

	require.Equal(t, "foo", srcs[2].Info.Filename)
	require.Equal(t, []byte("data1"), srcs[2].Info.Data)
	require.Nil(t, srcs[2].Info.Definition)

	require.Equal(t, 1, len(srcs[2].Ranges))
	require.Equal(t, int32(7), srcs[2].Ranges[0].Start.Line)
	require.Equal(t, int32(0), srcs[2].Ranges[0].Start.Character)
}

func testSourceMapFromRef(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	srcState := llb.Scratch().File(
		llb.Mkfile("foo", 0600, []byte("data")))
	sm := llb.NewSourceMap(&srcState, "bar", "mylang", []byte("bardata"))

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.Scratch().File(
			llb.Mkdir("foo/bar", 0600), // fails because /foo doesn't exist
			sm.Location([]*pb.Range{{Start: &pb.Position{Line: 3, Character: 1}}}),
		)

		def, err := st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		res, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}

		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}

		st2, err := ref.ToState()
		if err != nil {
			return nil, err
		}

		st = llb.Scratch().File(
			llb.Copy(st2, "foo", "foo2"),
		)

		def, err = st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}

	_, err = c.Build(sb.Context(), SolveOpt{}, "", frontend, nil)
	require.Error(t, err)

	srcs := errdefs.Sources(err)
	require.Equal(t, 1, len(srcs))

	require.Equal(t, "bar", srcs[0].Info.Filename)
	require.Equal(t, "mylang", srcs[0].Info.Language)
	require.Equal(t, []byte("bardata"), srcs[0].Info.Data)
	require.NotNil(t, srcs[0].Info.Definition)

	require.Equal(t, 1, len(srcs[0].Ranges))
	require.Equal(t, int32(3), srcs[0].Ranges[0].Start.Line)
	require.Equal(t, int32(1), srcs[0].Ranges[0].Start.Character)
}

func testRmSymlink(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// Test that if FileOp.Rm is called on a symlink, then
	// the symlink is removed rather than the target
	mnt := llb.Image("alpine").
		Run(llb.Shlex("touch /mnt/target")).
		AddMount("/mnt", llb.Scratch())

	mnt = llb.Image("alpine").
		Run(llb.Shlex("ln -s target /mnt/link")).
		AddMount("/mnt", mnt)

	def, err := mnt.File(llb.Rm("link")).Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.NoError(t, fstest.CheckDirectoryEqualWithApplier(destDir, fstest.CreateFile("target", nil, 0644)))
}

func testProxyEnv(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	scratch := func() llb.State {
		return integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))
	}
	base := llb.Image(imgName).Dir("/out")
	cmd := integration.UnixOrWindows(
		`sh -c "echo -n $HTTP_PROXY-$HTTPS_PROXY-$NO_PROXY-$no_proxy-$ALL_PROXY-$all_proxy > env"`,
		`cmd /C echo %HTTP_PROXY%-%HTTPS_PROXY%-%NO_PROXY%-%no_proxy%-%ALL_PROXY%-%all_proxy% > env`,
	)

	st := base.Run(llb.Shlex(cmd), llb.WithProxy(llb.ProxyEnv{
		HTTPProxy:  "httpvalue",
		HTTPSProxy: "httpsvalue",
		NoProxy:    "noproxyvalue",
		AllProxy:   "allproxyvalue",
	}))

	out := st.AddMount("/out", scratch())

	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "env"))
	require.NoError(t, err)
	newLine := integration.UnixOrWindows("", " \r\n")
	require.Equal(t, "httpvalue-httpsvalue-noproxyvalue-noproxyvalue-allproxyvalue-allproxyvalue"+newLine, string(dt))

	// repeat to make sure proxy doesn't change cache
	st = base.Run(llb.Shlex(cmd), llb.WithProxy(llb.ProxyEnv{
		HTTPSProxy: "httpsvalue2",
		NoProxy:    "noproxyvalue2",
	}))
	out = st.AddMount("/out", scratch())

	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)

	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err = os.ReadFile(filepath.Join(destDir, "env"))
	require.NoError(t, err)
	require.Equal(t, "httpvalue-httpsvalue-noproxyvalue-noproxyvalue-allproxyvalue-allproxyvalue"+newLine, string(dt))
}

func testMergeOp(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureMergeDiff)
	requiresLinux(t)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	ctx := sb.Context()
	registry, err := sb.NewRegistry()
	if !errors.Is(err, integration.ErrRequirements) {
		require.NoError(t, err)
	}

	var imageTarget string
	if workers.IsTestDockerdMoby(sb) {
		// do image export but use a fake url as the image should just end up in moby's
		// local store
		imageTarget = "fake.invalid:33333/buildkit/testmergeop:latest"
	} else if registry != "" {
		imageTarget = registry + "/buildkit/testmergeop:latest"
	}

	stateA := llb.Scratch().
		File(llb.Mkfile("/foo", 0755, []byte("A"))).
		File(llb.Mkfile("/a", 0755, []byte("A"))).
		File(llb.Mkdir("/bar", 0700)).
		File(llb.Mkfile("/bar/A", 0755, []byte("A")))
	stateB := stateA.
		File(llb.Rm("/foo")).
		File(llb.Mkfile("/b", 0755, []byte("B"))).
		File(llb.Mkfile("/bar/B", 0754, []byte("B")))
	stateC := llb.Scratch().
		File(llb.Mkfile("/foo", 0755, []byte("C"))).
		File(llb.Mkfile("/c", 0755, []byte("C"))).
		File(llb.Mkdir("/bar", 0755)).
		File(llb.Mkfile("/bar/A", 0400, []byte("C")))

	mergeA := llb.Merge([]llb.State{stateA, stateC})
	requireContents(ctx, t, c, sb, mergeA, nil, nil, imageTarget,
		fstest.CreateFile("foo", []byte("C"), 0755),
		fstest.CreateFile("c", []byte("C"), 0755),
		fstest.CreateDir("bar", 0755),
		fstest.CreateFile("bar/A", []byte("C"), 0400),
		fstest.CreateFile("a", []byte("A"), 0755),
	)

	mergeB := llb.Merge([]llb.State{stateC, stateB})
	requireContents(ctx, t, c, sb, mergeB, nil, nil, imageTarget,
		fstest.CreateFile("a", []byte("A"), 0755),
		fstest.CreateFile("b", []byte("B"), 0755),
		fstest.CreateFile("c", []byte("C"), 0755),
		fstest.CreateDir("bar", 0700),
		fstest.CreateFile("bar/A", []byte("A"), 0755),
		fstest.CreateFile("bar/B", []byte("B"), 0754),
	)

	stateD := llb.Scratch().File(llb.Mkdir("/qaz", 0755))
	mergeC := llb.Merge([]llb.State{mergeA, mergeB, stateD})
	requireContents(ctx, t, c, sb, mergeC, nil, nil, imageTarget,
		fstest.CreateFile("a", []byte("A"), 0755),
		fstest.CreateFile("b", []byte("B"), 0755),
		fstest.CreateFile("c", []byte("C"), 0755),
		fstest.CreateDir("bar", 0700),
		fstest.CreateFile("bar/A", []byte("A"), 0755),
		fstest.CreateFile("bar/B", []byte("B"), 0754),
		fstest.CreateDir("qaz", 0755),
	)

	runA := runShell(llb.Merge([]llb.State{llb.Image("alpine"), mergeC}),
		// turn /a file into a dir, mv b and c into it
		"rm /a",
		"mkdir /a",
		"mv /b /c /a/",
		// remove+recreate /bar to make it opaque on overlay snapshotters
		"rm -rf /bar",
		"mkdir -m 0755 /bar",
		"echo -n D > /bar/D",
		// turn /qaz dir into a file
		"rm -rf /qaz",
		"touch /qaz",
	)
	stateE := llb.Scratch().
		File(llb.Mkfile("/foo", 0755, []byte("E"))).
		File(llb.Mkdir("/bar", 0755)).
		File(llb.Mkfile("/bar/A", 0755, []byte("A"))).
		File(llb.Mkfile("/bar/E", 0755, nil))
	mergeD := llb.Merge([]llb.State{stateE, runA})
	requireEqualContents(ctx, t, c, mergeD, llb.Image("alpine").
		File(llb.Mkdir("a", 0755)).
		File(llb.Mkfile("a/b", 0755, []byte("B"))).
		File(llb.Mkfile("a/c", 0755, []byte("C"))).
		File(llb.Mkdir("bar", 0755)).
		File(llb.Mkfile("bar/D", 0644, []byte("D"))).
		File(llb.Mkfile("bar/E", 0755, nil)).
		File(llb.Mkfile("qaz", 0644, nil)),
	)
	// /foo from stateE is not here because it is deleted in stateB, which is part of a submerge of mergeD
}

func testMergeOpCacheInline(t *testing.T, sb integration.Sandbox) {
	testMergeOpCache(t, sb, "inline")
}

func testMergeOpCacheMin(t *testing.T, sb integration.Sandbox) {
	testMergeOpCache(t, sb, "min")
}

func testMergeOpCacheMax(t *testing.T, sb integration.Sandbox) {
	testMergeOpCache(t, sb, "max")
}

func testMergeOpCache(t *testing.T, sb integration.Sandbox, mode string) {
	t.Helper()
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureMergeDiff)
	requiresLinux(t)

	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Skip("test requires containerd worker")
	}

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// push the busybox image to the mutable registry
	sourceImage := "busybox:latest"
	def, err := llb.Image(sourceImage).Marshal(sb.Context())
	require.NoError(t, err)

	busyboxTargetNoTag := registry + "/buildkit/testlazyimage:"
	busyboxTarget := busyboxTargetNoTag + "latest"
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   busyboxTarget,
					"push":                                   "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	imageService := client.ImageService()
	contentStore := client.ContentStore()

	busyboxImg, err := imageService.Get(ctx, busyboxTarget)
	require.NoError(t, err)

	busyboxManifest, err := images.Manifest(ctx, contentStore, busyboxImg.Target, nil)
	require.NoError(t, err)

	for _, layer := range busyboxManifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.NoError(t, err)
	}

	// clear all local state out
	err = imageService.Delete(ctx, busyboxImg.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	for _, layer := range busyboxManifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	// make a new merge that includes the lazy busybox as a base and exports inline cache
	input1 := llb.Scratch().
		File(llb.Mkdir("/dir", 0777)).
		File(llb.Mkfile("/dir/1", 0777, nil))
	input1Copy := llb.Scratch().File(llb.Copy(input1, "/dir/1", "/foo/1", &llb.CopyInfo{CreateDestPath: true}))

	// put random contents in the file to ensure it's not re-run later
	input2 := runShell(llb.Image("alpine:latest"),
		"mkdir /dir",
		"cat /dev/urandom | head -c 100 | sha256sum > /dir/2")
	input2Copy := llb.Scratch().File(llb.Copy(input2, "/dir/2", "/bar/2", &llb.CopyInfo{CreateDestPath: true}))

	merge := llb.Merge([]llb.State{llb.Image(busyboxTarget), input1Copy, input2Copy})

	def, err = merge.Marshal(sb.Context())
	require.NoError(t, err)

	target := registry + "/buildkit/testmerge:latest"
	cacheTarget := registry + "/buildkit/testmergecache:latest"

	var cacheExports []CacheOptionsEntry
	var cacheImports []CacheOptionsEntry
	switch mode {
	case "inline":
		cacheExports = []CacheOptionsEntry{{
			Type: "inline",
		}}
		cacheImports = []CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref": target,
			},
		}}
	case "min":
		cacheExports = []CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref": cacheTarget,
			},
		}}
		cacheImports = []CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref": cacheTarget,
			},
		}}
	case "max":
		cacheExports = []CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":  cacheTarget,
				"mode": "max",
			},
		}}
		cacheImports = []CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref": cacheTarget,
			},
		}}
	default:
		require.Fail(t, fmt.Sprintf("unknown cache mode: %s", mode))
	}

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                                   target,
					"push":                                   "true",
					"store":                                  "true",
					"unsafe-internal-store-allow-incomplete": "true",
				},
			},
		},
		CacheExports: cacheExports,
	}, nil)
	require.NoError(t, err)

	// verify that the busybox image stayed lazy
	for _, layer := range busyboxManifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	// get the random value at /bar/2
	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	bar2Contents, err := os.ReadFile(filepath.Join(destDir, "bar", "2"))
	require.NoError(t, err)

	// clear all local state out
	img, err := imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err := images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	// re-run the same build with cache imports and verify everything stays lazy
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type: ExporterImage,
			Attrs: map[string]string{
				"name":                                   target,
				"push":                                   "true",
				"store":                                  "true",
				"unsafe-internal-store-allow-incomplete": "true",
			},
		}},
		CacheImports: cacheImports,
		CacheExports: cacheExports,
	}, nil)
	require.NoError(t, err)

	// verify everything from before stayed lazy
	img, err = imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err = images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for i, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v for index %d (%s)", err, i, layer.Digest)
	}

	// re-run the build with a change only to input1 using the remote cache
	input1 = llb.Scratch().
		File(llb.Mkdir("/dir", 0777)).
		File(llb.Mkfile("/dir/1", 0444, nil))
	input1Copy = llb.Scratch().File(llb.Copy(input1, "/dir/1", "/foo/1", &llb.CopyInfo{CreateDestPath: true}))

	merge = llb.Merge([]llb.State{llb.Image(busyboxTarget), input1Copy, input2Copy})

	def, err = merge.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type: ExporterImage,
			Attrs: map[string]string{
				"name":                                   target,
				"push":                                   "true",
				"store":                                  "true",
				"unsafe-internal-store-allow-incomplete": "true",
			},
		}},
		CacheExports: cacheExports,
		CacheImports: cacheImports,
	}, nil)
	require.NoError(t, err)

	// verify everything from before stayed lazy except the middle layer for input1Copy
	img, err = imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err = images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for i, layer := range manifest.Layers {
		switch i {
		case 0, 2:
			// bottom and top layer should stay lazy as they didn't change
			_, err = contentStore.Info(ctx, layer.Digest)
			require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v for index %d", err, i)
		case 1:
			// middle layer had to be rebuilt, should exist locally
			_, err = contentStore.Info(ctx, layer.Digest)
			require.NoError(t, err)
		default:
			require.Fail(t, fmt.Sprintf("unexpected layer index %d", i))
		}
	}

	// check the random value at /bar/2 didn't change
	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: cacheImports,
	}, nil)
	require.NoError(t, err)

	newBar2Contents, err := os.ReadFile(filepath.Join(destDir, "bar", "2"))
	require.NoError(t, err)

	require.Equalf(t, bar2Contents, newBar2Contents, "bar/2 contents changed")

	// Now test the case with a layer on top of a merge.
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	mergePlusLayer := merge.File(llb.Mkfile("/3", 0444, nil))

	def, err = mergePlusLayer.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type: ExporterImage,
			Attrs: map[string]string{
				"name":                                   target,
				"push":                                   "true",
				"store":                                  "true",
				"unsafe-internal-store-allow-incomplete": "true",
			},
		}},
		CacheExports: cacheExports,
		CacheImports: cacheImports,
	}, nil)
	require.NoError(t, err)

	// check the random value at /bar/2 didn't change
	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: cacheImports,
	}, nil)
	require.NoError(t, err)

	newBar2Contents, err = os.ReadFile(filepath.Join(destDir, "bar", "2"))
	require.NoError(t, err)

	require.Equalf(t, bar2Contents, newBar2Contents, "bar/2 contents changed")

	// clear local state, repeat the build, verify everything stays lazy
	err = imageService.Delete(ctx, img.Name, images.SynchronousDelete())
	require.NoError(t, err)
	checkAllReleasable(t, c, sb, true)

	for _, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v", err)
	}

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type: ExporterImage,
			Attrs: map[string]string{
				"name":                                   target,
				"push":                                   "true",
				"store":                                  "true",
				"unsafe-internal-store-allow-incomplete": "true",
			},
		}},
		CacheImports: cacheImports,
		CacheExports: cacheExports,
	}, nil)
	require.NoError(t, err)

	img, err = imageService.Get(ctx, target)
	require.NoError(t, err)

	manifest, err = images.Manifest(ctx, contentStore, img.Target, nil)
	require.NoError(t, err)

	for i, layer := range manifest.Layers {
		_, err = contentStore.Info(ctx, layer.Digest)
		require.ErrorIs(t, err, cerrdefs.ErrNotFound, "unexpected error %v for index %d", err, i)
	}
}

func requireContents(ctx context.Context, t *testing.T, c *Client, sb integration.Sandbox, state llb.State, cacheImports, cacheExports []CacheOptionsEntry, imageTarget string, files ...fstest.Applier) {
	t.Helper()

	def, err := state.Marshal(ctx)
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(ctx, def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: cacheImports,
		CacheExports: cacheExports,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, fstest.CheckDirectoryEqualWithApplier(destDir, fstest.Apply(files...)))

	if imageTarget != "" {
		var exports []ExportEntry
		if workers.IsTestDockerdMoby(sb) {
			exports = []ExportEntry{{
				Type: "moby",
				Attrs: map[string]string{
					"name": imageTarget,
				},
			}}
		} else {
			exports = []ExportEntry{{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": imageTarget,
					"push": "true",
				},
			}}
		}

		_, err = c.Solve(ctx, def, SolveOpt{Exports: exports, CacheImports: cacheImports, CacheExports: cacheExports}, nil)
		require.NoError(t, err)
		resetState(t, c, sb)
		requireContents(ctx, t, c, sb, llb.Image(imageTarget, llb.ResolveModePreferLocal), cacheImports, nil, "", files...)
	}
}

func requireEqualContents(ctx context.Context, t *testing.T, c *Client, stateA, stateB llb.State) {
	t.Helper()

	defA, err := stateA.Marshal(ctx)
	require.NoError(t, err)

	destDirA := t.TempDir()

	_, err = c.Solve(ctx, defA, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDirA,
			},
		},
	}, nil)
	require.NoError(t, err)

	defB, err := stateB.Marshal(ctx)
	require.NoError(t, err)

	destDirB := t.TempDir()

	_, err = c.Solve(ctx, defB, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDirB,
			},
		},
	}, nil)
	require.NoError(t, err)

	require.NoError(t, fstest.CheckDirectoryEqual(destDirA, destDirB))
}

func runShellExecState(base llb.State, cmds ...string) llb.ExecState {
	return base.Run(llb.Args([]string{"sh", "-c", strings.Join(cmds, " && ")}))
}

func runShell(base llb.State, cmds ...string) llb.State {
	return runShellExecState(base, cmds...).Root()
}

func chainRunShells(base llb.State, cmdss ...[]string) llb.State {
	for _, cmds := range cmdss {
		base = runShell(base, cmds...)
	}
	return base
}

func requiresLinux(t *testing.T) {
	integration.SkipOnPlatform(t, "!linux")
}

// ensurePruneAll tries to ensure Prune completes with retries.
// Current cache implementation defers release-related logic using goroutine so
// there can be situation where a build has finished but the following prune doesn't
// cleanup cache because some records still haven't been released.
// This function tries to ensure prune by retrying it.
func ensurePruneAll(t *testing.T, c *Client, sb integration.Sandbox) {
	for i := range 2 {
		require.NoError(t, c.Prune(sb.Context(), nil, PruneAll))
		for range 20 {
			du, err := c.DiskUsage(sb.Context())
			require.NoError(t, err)
			if len(du) == 0 {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("retrying prune(%d)", i)
	}
	t.Fatalf("failed to ensure prune")
}

func checkAllReleasable(t *testing.T, c *Client, sb integration.Sandbox, checkContent bool) {
	cl, err := c.ControlClient().ListenBuildHistory(sb.Context(), &controlapi.BuildHistoryRequest{
		EarlyExit: true,
	})
	require.NoError(t, err)

	for {
		resp, err := cl.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		_, err = c.ControlClient().UpdateBuildHistory(sb.Context(), &controlapi.UpdateBuildHistoryRequest{
			Ref:    resp.Record.Ref,
			Delete: true,
		})
		require.NoError(t, err)
	}

	retries := 0
loop0:
	for {
		require.Greater(t, 20, retries)
		retries++
		du, err := c.DiskUsage(sb.Context())
		require.NoError(t, err)
		for _, d := range du {
			if d.InUse {
				time.Sleep(500 * time.Millisecond)
				continue loop0
			}
		}
		break
	}

	err = c.Prune(sb.Context(), nil, PruneAll)
	require.NoError(t, err)

	du, err := c.DiskUsage(sb.Context())
	require.NoError(t, err)
	require.Equal(t, 0, len(du))

	// examine contents of exported tars (requires containerd)
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		if checkContent {
			if err := workers.HasFeatureCompat(t, sb, workers.FeatureContentCheck); err == nil {
				store := proxy.NewContentStore(c.ContentClient())
				count := 0
				err := store.Walk(sb.Context(), func(info content.Info) error {
					count++
					return nil
				})
				require.NoError(t, err)
				require.Equal(t, 0, count)
			}
		}
		t.Logf("checkAllReleasable: skipping check for exported tars in non-containerd test")
		return
	}

	// TODO: make public pull helper function so this can be checked for standalone as well

	client, err := newContainerd(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	for _, ns := range []string{"buildkit", "buildkit_history"} {
		ctx := namespaces.WithNamespace(sb.Context(), ns)
		snapshotterName := sb.Snapshotter()
		snapshotService := client.SnapshotService(snapshotterName)

		leases, err := client.LeasesService().List(ctx)
		require.NoError(t, err)
		count := 0
		for _, l := range leases {
			_, isTemp := l.Labels["buildkit/lease.temporary"]
			_, isExpire := l.Labels["containerd.io/gc.expire"]
			if isTemp && isExpire {
				continue
			}
			count++
			t.Logf("lease: %v", l)
		}
		require.Equal(t, 0, count)

		if checkContent {
			images, err := client.ImageService().List(ctx)
			require.NoError(t, err)
			for _, img := range images {
				err := client.ImageService().Delete(ctx, img.Name)
				require.NoError(t, err)
			}
		}

		retries = 0
		for {
			count := 0
			err = snapshotService.Walk(ctx, func(context.Context, snapshots.Info) error {
				count++
				return nil
			})
			require.NoError(t, err)
			if count == 0 {
				break
			}
			require.Less(t, retries, 20)
			retries++
			time.Sleep(500 * time.Millisecond)
		}

		if !checkContent {
			return
		}

		retries = 0
		for {
			count := 0
			var infos []content.Info
			err = client.ContentStore().Walk(ctx, func(info content.Info) error {
				count++
				infos = append(infos, info)
				return nil
			})
			require.NoError(t, err)
			if count == 0 {
				break
			}
			if retries >= 50 {
				for _, info := range infos {
					t.Logf("content: %v %v %+v", info.Digest, info.Size, info.Labels)
					ra, err := client.ContentStore().ReaderAt(ctx, ocispecs.Descriptor{
						Digest: info.Digest,
						Size:   info.Size,
					})
					if err == nil {
						dt := make([]byte, 1024)
						n, err := ra.ReadAt(dt, 0)
						t.Logf("data: %+v %q", err, string(dt[:n]))
					}
				}
				require.FailNowf(t, "content still exists", "%+v", infos)
			}
			retries++
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func testInvalidExporter(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	def, err := llb.Image(imgName).Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	target := "example.com/buildkit/testoci:latest"
	attrs := map[string]string{
		"name": target,
	}
	for _, exp := range []string{ExporterOCI, ExporterDocker} {
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:  exp,
					Attrs: attrs,
				},
			},
		}, nil)
		// output file writer is required
		require.Error(t, err)
		_, err = c.Solve(sb.Context(), def, SolveOpt{
			Exports: []ExportEntry{
				{
					Type:      exp,
					Attrs:     attrs,
					OutputDir: destDir,
				},
			},
		}, nil)
		// output directory is not supported
		require.Error(t, err)
	}

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:  ExporterLocal,
				Attrs: attrs,
			},
		},
	}, nil)
	// output directory is required
	require.Error(t, err)

	f, err := os.Create(filepath.Join(destDir, "a"))
	require.NoError(t, err)
	defer f.Close()
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterLocal,
				Attrs:  attrs,
				Output: fixedWriteCloser(f),
			},
		},
	}, nil)
	// output file writer is not supported
	require.Error(t, err)

	checkAllReleasable(t, c, sb, true)
}

// moby/buildkit#492
func testParallelLocalBuilds(t *testing.T, sb integration.Sandbox) {
	ctx, cancel := context.WithCancelCause(sb.Context())
	defer func() { cancel(errors.WithStack(context.Canceled)) }()

	c, err := New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	eg, ctx := errgroup.WithContext(ctx)

	for i := range 3 {
		func(i int) {
			eg.Go(func() error {
				fn := fmt.Sprintf("test%d", i)
				srcDir := integration.Tmpdir(
					t,
					fstest.CreateFile(fn, []byte("contents"), 0600),
				)

				def, err := llb.Local("source").Marshal(sb.Context())
				require.NoError(t, err)

				destDir := t.TempDir()

				_, err = c.Solve(ctx, def, SolveOpt{
					Exports: []ExportEntry{
						{
							Type:      ExporterLocal,
							OutputDir: destDir,
						},
					},
					LocalMounts: map[string]fsutil.FS{
						"source": srcDir,
					},
				}, nil)
				require.NoError(t, err)

				act, err := os.ReadFile(filepath.Join(destDir, fn))
				require.NoError(t, err)

				require.Equal(t, "contents", string(act))
				return nil
			})
		}(i)
	}

	err = eg.Wait()
	require.NoError(t, err)
}

func testMetadataOnlyLocal(t *testing.T, sb integration.Sandbox) {
	ctx, cancel := context.WithCancelCause(sb.Context())
	defer func() { cancel(errors.WithStack(context.Canceled)) }()

	c, err := New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	srcDir := integration.Tmpdir(
		t,
		fstest.CreateFile("data", []byte("contents"), 0600),
		fstest.CreateDir("dir", 0700),
		fstest.CreateFile("dir/file1", []byte("file1"), 0600),
		fstest.CreateFile("dir/file2", []byte("file2"), 0600),
		fstest.CreateDir("dir/subdir", 0700),
		fstest.CreateDir("dir/subdir2", 0700),
		fstest.CreateFile("dir/subdir/bar1", []byte("bar1"), 0600),
		fstest.CreateFile("dir/subdir/bar2", []byte("bar2"), 0600),
		fstest.CreateFile("dir/subdir2/bar3", []byte("bar3"), 0600),
		fstest.CreateFile("foo", []byte("foo"), 0602),
	)

	def, err := llb.Local("source", llb.MetadataOnlyTransfer([]string{"dir/**/*1", "foo"})).Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(ctx, def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"source": srcDir,
		},
	}, nil)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(destDir, "data"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	act, err := os.ReadFile(filepath.Join(destDir, "dir/file1"))
	require.NoError(t, err)
	require.Equal(t, "file1", string(act))

	act, err = os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, "foo", string(act))

	_, err = os.ReadFile(filepath.Join(destDir, "dir/file2"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	act, err = os.ReadFile(filepath.Join(destDir, "dir/subdir/bar1"))
	require.NoError(t, err)
	require.Equal(t, "bar1", string(act))

	_, err = os.Stat(filepath.Join(destDir, "dir/subdir2"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	_, err = os.ReadFile(filepath.Join(destDir, "dir/subdir/bar2"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	dt, err := os.ReadFile(filepath.Join(destDir, ".fsutil-metadata"))
	require.NoError(t, err)

	stats := parseFSMetadata(t, dt)
	require.Equal(t, 10, len(stats))

	require.Equal(t, "data", stats[0].Path)
	require.Equal(t, "dir", stats[1].Path)
	require.Equal(t, "dir/file1", stats[2].Path)
	require.Equal(t, "dir/file2", stats[3].Path)
	require.Equal(t, "dir/subdir", stats[4].Path)
	require.Equal(t, "dir/subdir/bar1", stats[5].Path)
	require.Equal(t, "dir/subdir/bar2", stats[6].Path)
	require.Equal(t, "dir/subdir2", stats[7].Path)
	require.Equal(t, "dir/subdir2/bar3", stats[8].Path)
	require.Equal(t, "foo", stats[9].Path)

	err = os.RemoveAll(filepath.Join(srcDir.Name, "dir/subdir"))
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(srcDir.Name, "dir/file1"), []byte("file1-updated"), 0600)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(srcDir.Name, "dir/bar1"), []byte("bar1"), 0600)
	require.NoError(t, err)

	def, err = llb.Local("source", llb.MetadataOnlyTransfer([]string{"dir/**/*1", "foo"})).Marshal(sb.Context())
	require.NoError(t, err)

	destDir = t.TempDir()

	_, err = c.Solve(ctx, def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		LocalMounts: map[string]fsutil.FS{
			"source": srcDir,
		},
	}, nil)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(destDir, "data"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	act, err = os.ReadFile(filepath.Join(destDir, "dir/file1"))
	require.NoError(t, err)
	require.Equal(t, "file1-updated", string(act))

	act, err = os.ReadFile(filepath.Join(destDir, "dir/bar1"))
	require.NoError(t, err)
	require.Equal(t, "bar1", string(act))

	_, err = os.Stat(filepath.Join(destDir, "dir/subdir"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	dt, err = os.ReadFile(filepath.Join(destDir, ".fsutil-metadata"))
	require.NoError(t, err)

	stats = parseFSMetadata(t, dt)
	require.Equal(t, 8, len(stats))

	require.Equal(t, "data", stats[0].Path)
	require.Equal(t, "dir", stats[1].Path)
	require.Equal(t, "dir/bar1", stats[2].Path)
	require.Equal(t, "dir/file1", stats[3].Path)
	require.Equal(t, "dir/file2", stats[4].Path)
	require.Equal(t, "dir/subdir2", stats[5].Path)
	require.Equal(t, "dir/subdir2/bar3", stats[6].Path)
	require.Equal(t, "foo", stats[7].Path)
}

// testRelativeMountpoint is a test that relative paths for mountpoints don't
// fail when runc is upgraded to at least rc95, which introduces an error when
// mountpoints are not absolute. Relative paths should be transformed to
// absolute points based on the llb.State's current working directory.
func testRelativeMountpoint(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	id := identity.NewID()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	scratch := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))
	cmdStr := integration.UnixOrWindows(
		"sh -c 'echo -n %s > /root/relpath/data'",
		`cmd /C echo %s > \\root\\relpath\\data`,
	)
	st := llb.Image(imgName).Dir("/root").Run(
		llb.Shlexf(cmdStr, id),
	).AddMount("relpath", scratch)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "data"))
	require.NoError(t, err)
	newLine := integration.UnixOrWindows("", " \r\n")
	require.Equal(t, dt, []byte(id+newLine))
}

func testPullWithLayerLimit(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Scratch().
		File(llb.Mkfile("/first", 0644, []byte("first"))).
		File(llb.Mkfile("/second", 0644, []byte("second"))).
		File(llb.Mkfile("/third", 0644, []byte("third")))

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/testlayers:latest"

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// pull 2 first layers
	st = llb.Image(target, llb.WithLayerLimit(2)).
		File(llb.Mkfile("/forth", 0644, []byte("forth")))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir := t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type:      ExporterLocal,
			OutputDir: destDir,
		}},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "first"))
	require.NoError(t, err)
	require.Equal(t, "first", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "second"))
	require.NoError(t, err)
	require.Equal(t, "second", string(dt))

	_, err = os.ReadFile(filepath.Join(destDir, "third"))
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))

	dt, err = os.ReadFile(filepath.Join(destDir, "forth"))
	require.NoError(t, err)
	require.Equal(t, "forth", string(dt))

	// pull 3rd layer only
	st = llb.Diff(
		llb.Image(target, llb.WithLayerLimit(2)),
		llb.Image(target)).
		File(llb.Mkfile("/forth", 0644, []byte("forth")))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	destDir = t.TempDir()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{{
			Type:      ExporterLocal,
			OutputDir: destDir,
		}},
	}, nil)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(destDir, "first"))
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))

	_, err = os.ReadFile(filepath.Join(destDir, "second"))
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))

	dt, err = os.ReadFile(filepath.Join(destDir, "third"))
	require.NoError(t, err)
	require.Equal(t, "third", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "forth"))
	require.NoError(t, err)
	require.Equal(t, "forth", string(dt))

	// zero limit errors cleanly
	st = llb.Image(target, llb.WithLayerLimit(0))

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid layer limit")
}

func testCallInfo(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureInfo)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Info(sb.Context())
	require.NoError(t, err)
}

func testValidateDigestOrigin(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	scratch := integration.UnixOrWindows(llb.Scratch(), llb.Image(imgName))

	cmdStr := integration.UnixOrWindows(
		"touch foo",
		"cmd /C echo a > foo",
	)
	st := llb.Image(imgName).
		Run(llb.Shlex(cmdStr), llb.Dir("/wd")).
		AddMount("/wd", scratch)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/testdigest:latest"

	resp, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.True(t, ok)

	err = c.Prune(sb.Context(), nil, PruneAll)
	require.NoError(t, err)

	st = llb.Image(target + "@" + dgst)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// accessing the digest from invalid names should fail
	st = llb.Image("example.invalid/nosuchrepo@" + dgst)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)

	// also check repo that does exists but not digest
	st = llb.Image("docker.io/library/ubuntu@" + dgst)

	def, err = st.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
}

func testExportAnnotations(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureOCIExporter)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	scratch := integration.UnixOrWindows(llb.Scratch(), llb.Image("nanoserver"))

	amd64 := platforms.MustParse("linux/amd64")
	arm64 := platforms.MustParse("linux/arm64")
	ps := []ocispecs.Platform{amd64, arm64}

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(ps)),
		}
		for i, p := range ps {
			st := scratch.File(
				llb.Mkfile("platform", 0600, []byte(platforms.Format(p))),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}

			k := platforms.Format(p)
			res.AddRef(k, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       k,
				Platform: p,
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		res.AddMeta(exptypes.AnnotationIndexKey("gi"), []byte("generic index"))
		res.AddMeta(exptypes.AnnotationIndexDescriptorKey("gid"), []byte("generic index descriptor"))
		res.AddMeta(exptypes.AnnotationManifestKey(nil, "gm"), []byte("generic manifest"))
		res.AddMeta(exptypes.AnnotationManifestDescriptorKey(nil, "gmd"), []byte("generic manifest descriptor"))
		res.AddMeta(exptypes.AnnotationManifestKey(&amd64, "m"), []byte("amd64 manifest"))
		res.AddMeta(exptypes.AnnotationManifestKey(&arm64, "m"), []byte("arm64 manifest"))
		res.AddMeta(exptypes.AnnotationManifestDescriptorKey(&amd64, "md"), []byte("amd64 manifest descriptor"))
		res.AddMeta(exptypes.AnnotationManifestDescriptorKey(&arm64, "md"), []byte("arm64 manifest descriptor"))
		res.AddMeta(exptypes.AnnotationKey{Key: "gd"}.String(), []byte("generic default"))

		return res, nil
	}

	// testing for image exporter

	target := registry + "/buildkit/testannotations:latest"

	const created = "2022-01-23T12:34:56Z"

	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                 target,
					"push":                 "true",
					"annotation-index.gio": "generic index opt",
					"annotation-index." + ocispecs.AnnotationCreated:  created,
					"annotation-manifest.gmo":                         "generic manifest opt",
					"annotation-manifest-descriptor.gmdo":             "generic manifest descriptor opt",
					"annotation-manifest[linux/amd64].mo":             "amd64 manifest opt",
					"annotation-manifest-descriptor[linux/amd64].mdo": "amd64 manifest descriptor opt",
					"annotation-manifest[linux/arm64].mo":             "arm64 manifest opt",
					"annotation-manifest-descriptor[linux/arm64].mdo": "arm64 manifest descriptor opt",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	require.Equal(t, "generic index", imgs.Index.Annotations["gi"])
	require.Equal(t, "generic index opt", imgs.Index.Annotations["gio"])
	require.Equal(t, created, imgs.Index.Annotations[ocispecs.AnnotationCreated])
	for _, desc := range imgs.Index.Manifests {
		require.Equal(t, "generic manifest descriptor", desc.Annotations["gmd"])
		require.Equal(t, "generic manifest descriptor opt", desc.Annotations["gmdo"])
		switch {
		case platforms.Only(amd64).Match(*desc.Platform):
			require.Equal(t, "amd64 manifest descriptor", desc.Annotations["md"])
			require.Equal(t, "amd64 manifest descriptor opt", desc.Annotations["mdo"])
		case platforms.Only(arm64).Match(*desc.Platform):
			require.Equal(t, "arm64 manifest descriptor", desc.Annotations["md"])
			require.Equal(t, "arm64 manifest descriptor opt", desc.Annotations["mdo"])
		default:
			require.Fail(t, "unrecognized platform")
		}
	}

	amdImage := imgs.Find(platforms.Format(amd64))
	require.Equal(t, "generic default", amdImage.Manifest.Annotations["gd"])
	require.Equal(t, "generic manifest", amdImage.Manifest.Annotations["gm"])
	require.Equal(t, "generic manifest opt", amdImage.Manifest.Annotations["gmo"])
	require.Equal(t, "amd64 manifest", amdImage.Manifest.Annotations["m"])
	require.Equal(t, "amd64 manifest opt", amdImage.Manifest.Annotations["mo"])

	armImage := imgs.Find(platforms.Format(arm64))
	require.Equal(t, "generic default", armImage.Manifest.Annotations["gd"])
	require.Equal(t, "generic manifest", armImage.Manifest.Annotations["gm"])
	require.Equal(t, "generic manifest opt", armImage.Manifest.Annotations["gmo"])
	require.Equal(t, "arm64 manifest", armImage.Manifest.Annotations["m"])
	require.Equal(t, "arm64 manifest opt", armImage.Manifest.Annotations["mo"])

	// testing for oci exporter

	destDir := t.TempDir()

	out := filepath.Join(destDir, "out.tar")
	outW, err := os.Create(out)
	require.NoError(t, err)

	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterOCI,
				Output: fixedWriteCloser(outW),
				Attrs: map[string]string{
					"annotation-index.gio":                                      "generic index opt",
					"annotation-index-descriptor.gido":                          "generic index descriptor opt",
					"annotation-index-descriptor." + ocispecs.AnnotationCreated: created,
					"annotation-manifest.gmo":                                   "generic manifest opt",
					"annotation-manifest-descriptor.gmdo":                       "generic manifest descriptor opt",
					"annotation-manifest[linux/amd64].mo":                       "amd64 manifest opt",
					"annotation-manifest-descriptor[linux/amd64].mdo":           "amd64 manifest descriptor opt",
					"annotation-manifest[linux/arm64].mo":                       "arm64 manifest opt",
					"annotation-manifest-descriptor[linux/arm64].mdo":           "arm64 manifest descriptor opt",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(out)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	var layout ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &layout)
	require.Equal(t, "generic index descriptor", layout.Manifests[0].Annotations["gid"])
	require.Equal(t, "generic index descriptor opt", layout.Manifests[0].Annotations["gido"])
	require.Equal(t, created, layout.Manifests[0].Annotations[ocispecs.AnnotationCreated])
	require.NoError(t, err)

	var index ocispecs.Index
	err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+layout.Manifests[0].Digest.Hex()].Data, &index)
	require.Equal(t, "generic index", index.Annotations["gi"])
	require.Equal(t, "generic index opt", index.Annotations["gio"])
	require.NoError(t, err)

	for _, desc := range index.Manifests {
		var mfst ocispecs.Manifest
		err = json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+desc.Digest.Hex()].Data, &mfst)
		require.NoError(t, err)

		require.Equal(t, "generic default", mfst.Annotations["gd"])
		require.Equal(t, "generic manifest", mfst.Annotations["gm"])
		require.Equal(t, "generic manifest descriptor", desc.Annotations["gmd"])
		require.Equal(t, "generic manifest opt", mfst.Annotations["gmo"])
		require.Equal(t, "generic manifest descriptor opt", desc.Annotations["gmdo"])

		switch {
		case platforms.Only(amd64).Match(*desc.Platform):
			require.Equal(t, "amd64 manifest", mfst.Annotations["m"])
			require.Equal(t, "amd64 manifest descriptor", desc.Annotations["md"])
			require.Equal(t, "amd64 manifest opt", mfst.Annotations["mo"])
			require.Equal(t, "amd64 manifest descriptor opt", desc.Annotations["mdo"])
		case platforms.Only(arm64).Match(*desc.Platform):
			require.Equal(t, "arm64 manifest", mfst.Annotations["m"])
			require.Equal(t, "arm64 manifest descriptor", desc.Annotations["md"])
			require.Equal(t, "arm64 manifest opt", mfst.Annotations["mo"])
			require.Equal(t, "arm64 manifest descriptor opt", desc.Annotations["mdo"])
		default:
			require.Fail(t, "unrecognized platform")
		}
	}
}

func testExportAnnotationsMediaTypes(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	p := platforms.DefaultSpec()
	ps := []ocispecs.Platform{p}

	scratch := integration.UnixOrWindows(llb.Scratch(), llb.Image("nanoserver"))

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(ps)),
		}
		for i, p := range ps {
			st := scratch.File(
				llb.Mkfile("platform", 0600, []byte(platforms.Format(p))),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}

			k := platforms.Format(p)
			res.AddRef(k, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       k,
				Platform: p,
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	target := registry + "/buildkit/testannotationsmedia:1"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":                  target,
					"push":                  "true",
					"annotation-manifest.a": "b",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 1, len(imgs.Images))

	target2 := registry + "/buildkit/testannotationsmedia:2"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name":               target2,
					"push":               "true",
					"annotation-index.c": "d",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target2)
	require.NoError(t, err)
	imgs2, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 1, len(imgs2.Images))

	require.Equal(t, "b", imgs.Images[0].Manifest.Annotations["a"])
	require.Equal(t, "d", imgs2.Index.Annotations["c"])

	require.Equal(t, images.MediaTypeDockerSchema2ManifestList, imgs.Index.MediaType)
	require.Equal(t, ocispecs.MediaTypeImageIndex, imgs2.Index.MediaType)
}

func testExportAttestationsOCIArtifact(t *testing.T, sb integration.Sandbox) {
	testExportAttestations(t, sb, true)
}

func testExportAttestationsImageManifest(t *testing.T, sb integration.Sandbox) {
	testExportAttestations(t, sb, false)
}

func testExportAttestations(t *testing.T, sb integration.Sandbox, ociArtifact bool) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	ps := []ocispecs.Platform{
		platforms.MustParse("linux/amd64"),
		platforms.MustParse("linux/arm64"),
	}

	success := []byte(`{"success": true}`)
	successDigest := digest.SHA256.FromBytes(success)

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{}

		for _, p := range ps {
			pk := platforms.Format(p)
			expPlatforms.Platforms = append(expPlatforms.Platforms, exptypes.Platform{ID: pk, Platform: p})

			// build image
			st := llb.Scratch().File(
				llb.Mkfile("/greeting", 0600, fmt.Appendf(nil, "hello %s!", pk)),
			)
			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(pk, ref)

			// build attestations
			st = llb.Scratch().
				File(llb.Mkfile("/attestation.json", 0600, success)).
				File(llb.Mkfile("/attestation2.json", 0600, []byte{}))
			def, err = st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err = c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			refAttest, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddAttestation(pk, gateway.Attestation{
				Kind: gatewaypb.AttestationKind_InToto,
				Ref:  refAttest,
				Path: "/attestation.json",
				InToto: result.InTotoAttestation{
					PredicateType: "https://example.com/attestations/v1.0",
					Subjects: []result.InTotoSubject{{
						Kind: gatewaypb.InTotoSubjectKind_Self,
					}},
				},
			})
			res.AddAttestation(pk, gateway.Attestation{
				Kind: gatewaypb.AttestationKind_InToto,
				Ref:  refAttest,
				Path: "/attestation2.json",
				InToto: result.InTotoAttestation{
					PredicateType: "https://example.com/attestations2/v1.0",
					Subjects: []result.InTotoSubject{{
						Kind:   gatewaypb.InTotoSubjectKind_Raw,
						Name:   "/attestation.json",
						Digest: []digest.Digest{successDigest},
					}},
				},
			})
		}

		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	t.Run("image", func(t *testing.T) {
		targets := []string{
			registry + "/buildkit/testattestationsfoo:latest",
			registry + "/buildkit/testattestationsbar:latest",
		}
		_, err = c.Build(sb.Context(), SolveOpt{
			Exports: []ExportEntry{
				{
					Type: ExporterImage,
					Attrs: map[string]string{
						"name":         strings.Join(targets, ","),
						"push":         "true",
						"oci-artifact": strconv.FormatBool(ociArtifact),
					},
				},
			},
		}, "", frontend, nil)
		require.NoError(t, err)

		desc, provider, err := contentutil.ProviderFromRef(targets[0])
		require.NoError(t, err)

		imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
		require.NoError(t, err)
		require.Equal(t, len(ps)*2, len(imgs.Images))

		var bases []*testutil.ImageInfo
		for _, p := range ps {
			pk := platforms.Format(p)
			img := imgs.Find(pk)
			require.NotNil(t, img)
			require.Equal(t, pk, platforms.Format(*img.Desc.Platform))
			require.Equal(t, 1, len(img.Layers))
			require.Equal(t, fmt.Appendf(nil, "hello %s!", pk), img.Layers[0]["greeting"].Data)
			bases = append(bases, img)
		}

		atts := imgs.Filter("unknown/unknown")
		require.Equal(t, len(ps), len(atts.Images))
		for i, att := range atts.Images {
			require.Equal(t, ocispecs.MediaTypeImageManifest, att.Desc.MediaType)
			require.Equal(t, "unknown/unknown", platforms.Format(*att.Desc.Platform))
			require.Equal(t, attestation.DockerAnnotationReferenceTypeDefault, att.Desc.Annotations[attestation.DockerAnnotationReferenceType])
			require.Equal(t, bases[i].Desc.Digest.String(), att.Desc.Annotations[attestation.DockerAnnotationReferenceDigest])
			require.Equal(t, 2, len(att.Layers))

			if ociArtifact {
				subject := att.Manifest.Subject
				require.NotNil(t, subject)
				require.Equal(t, bases[i].Desc, *subject)
				require.Equal(t, "application/vnd.docker.attestation.manifest.v1+json", att.Manifest.ArtifactType)
				require.Equal(t, ocispecs.DescriptorEmptyJSON, att.Manifest.Config)
			} else {
				require.Nil(t, att.Manifest.Subject)
				require.Empty(t, att.Manifest.ArtifactType)

				// image config is not included in the OCI artifact
				require.Equal(t, "unknown/unknown", att.Img.OS+"/"+att.Img.Architecture)
				require.Equal(t, len(att.Layers), len(att.Img.RootFS.DiffIDs))
				require.Equal(t, 0, len(att.Img.History))
			}

			var attest intoto.Statement
			require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))

			purls := map[string]string{}
			for _, k := range targets {
				named, err := reference.ParseNormalizedNamed(k)
				require.NoError(t, err)
				name := reference.FamiliarName(named)
				version := ""
				if tagged, ok := named.(reference.Tagged); ok {
					version = tagged.Tag()
				}
				p := fmt.Sprintf("pkg:docker/%s%s@%s?platform=%s", url.QueryEscape(registry), strings.TrimPrefix(name, registry), version, url.PathEscape(platforms.Format(ps[i])))
				purls[k] = p
			}

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
			require.Equal(t, "https://example.com/attestations/v1.0", attest.PredicateType)
			require.Equal(t, map[string]any{"success": true}, attest.Predicate)
			subjects := []intoto.Subject{
				{
					Name: purls[targets[0]],
					Digest: map[string]string{
						"sha256": bases[i].Desc.Digest.Encoded(),
					},
				},
				{
					Name: purls[targets[1]],
					Digest: map[string]string{
						"sha256": bases[i].Desc.Digest.Encoded(),
					},
				},
			}
			require.Equal(t, subjects, attest.Subject)

			var attest2 intoto.Statement
			require.NoError(t, json.Unmarshal(att.LayersRaw[1], &attest2))

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest2.Type)
			require.Equal(t, "https://example.com/attestations2/v1.0", attest2.PredicateType)
			require.Nil(t, attest2.Predicate)
			subjects = []intoto.Subject{{
				Name: "/attestation.json",
				Digest: map[string]string{
					"sha256": successDigest.Encoded(),
				},
			}}
			require.Equal(t, subjects, attest2.Subject)
		}

		cdAddress := sb.ContainerdAddress()
		if cdAddress == "" {
			return
		}
		client, err := ctd.New(cdAddress)
		require.NoError(t, err)
		defer client.Close()
		ctx := namespaces.WithNamespace(sb.Context(), "buildkit")

		for _, target := range targets {
			err = client.ImageService().Delete(ctx, target, images.SynchronousDelete())
			require.NoError(t, err)
		}
		checkAllReleasable(t, c, sb, true)
	})

	t.Run("local", func(t *testing.T) {
		dir := t.TempDir()
		_, err = c.Build(sb.Context(), SolveOpt{
			Exports: []ExportEntry{
				{
					Type:      ExporterLocal,
					OutputDir: dir,
					Attrs: map[string]string{
						"attestation-prefix": "test.",
					},
				},
			},
		}, "", frontend, nil)
		require.NoError(t, err)

		for _, p := range ps {
			var attest intoto.Statement
			dt, err := os.ReadFile(path.Join(dir, strings.ReplaceAll(platforms.Format(p), "/", "_"), "test.attestation.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(dt, &attest))

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
			require.Equal(t, "https://example.com/attestations/v1.0", attest.PredicateType)
			require.Equal(t, map[string]any{"success": true}, attest.Predicate)

			require.Equal(t, []intoto.Subject{{
				Name:   "greeting",
				Digest: result.ToDigestMap(digest.Canonical.FromString("hello " + platforms.Format(p) + "!")),
			}}, attest.Subject)

			var attest2 intoto.Statement
			dt, err = os.ReadFile(path.Join(dir, strings.ReplaceAll(platforms.Format(p), "/", "_"), "test.attestation2.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(dt, &attest2))

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest2.Type)
			require.Equal(t, "https://example.com/attestations2/v1.0", attest2.PredicateType)
			require.Nil(t, attest2.Predicate)
			subjects := []intoto.Subject{{
				Name: "/attestation.json",
				Digest: map[string]string{
					"sha256": successDigest.Encoded(),
				},
			}}
			require.Equal(t, subjects, attest2.Subject)
		}
	})

	t.Run("tar", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "out.tar")
		outW, err := os.Create(out)
		require.NoError(t, err)

		_, err = c.Build(sb.Context(), SolveOpt{
			Exports: []ExportEntry{
				{
					Type:   ExporterTar,
					Output: fixedWriteCloser(outW),
					Attrs: map[string]string{
						"attestation-prefix": "test.",
					},
				},
			},
		}, "", frontend, nil)
		require.NoError(t, err)

		dt, err := os.ReadFile(out)
		require.NoError(t, err)

		m, err := testutil.ReadTarToMap(dt, false)
		require.NoError(t, err)

		for _, p := range ps {
			var attest intoto.Statement
			item := m[path.Join(strings.ReplaceAll(platforms.Format(p), "/", "_"), "test.attestation.json")]
			require.NotNil(t, item)
			require.NoError(t, json.Unmarshal(item.Data, &attest))

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
			require.Equal(t, "https://example.com/attestations/v1.0", attest.PredicateType)
			require.Equal(t, map[string]any{"success": true}, attest.Predicate)

			require.Equal(t, []intoto.Subject{{
				Name:   "greeting",
				Digest: result.ToDigestMap(digest.Canonical.FromString("hello " + platforms.Format(p) + "!")),
			}}, attest.Subject)

			var attest2 intoto.Statement
			item = m[path.Join(strings.ReplaceAll(platforms.Format(p), "/", "_"), "test.attestation2.json")]
			require.NotNil(t, item)
			require.NoError(t, json.Unmarshal(item.Data, &attest2))

			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest2.Type)
			require.Equal(t, "https://example.com/attestations2/v1.0", attest2.PredicateType)
			require.Nil(t, attest2.Predicate)
			subjects := []intoto.Subject{{
				Name: "/attestation.json",
				Digest: map[string]string{
					"sha256": successDigest.Encoded(),
				},
			}}
			require.Equal(t, subjects, attest2.Subject)
		}
	})
}

func testAttestationDefaultSubject(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	ps := []ocispecs.Platform{
		platforms.MustParse("linux/amd64"),
	}

	success := []byte(`{"success": true}`)
	scratch := func() llb.State {
		return integration.UnixOrWindows(llb.Scratch(), llb.Image("nanoserver"))
	}

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{}

		for _, p := range ps {
			pk := platforms.Format(p)
			expPlatforms.Platforms = append(expPlatforms.Platforms, exptypes.Platform{ID: pk, Platform: p})

			// build image
			st := scratch().File(
				llb.Mkfile("/greeting", 0600, fmt.Appendf(nil, "hello %s!", pk)),
			)
			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(pk, ref)

			// build attestations
			st = scratch().File(llb.Mkfile("/attestation.json", 0600, success))
			def, err = st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err = c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			refAttest, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddAttestation(pk, gateway.Attestation{
				Kind: gatewaypb.AttestationKind_InToto,
				Ref:  refAttest,
				Path: "/attestation.json",
				InToto: result.InTotoAttestation{
					PredicateType: "https://example.com/attestations/v1.0",
				},
			})
		}

		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	target := registry + "/buildkit/testattestationsemptysubject:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, len(ps)*2, len(imgs.Images))

	var bases []*testutil.ImageInfo
	for _, p := range ps {
		pk := platforms.Format(p)
		bases = append(bases, imgs.Find(pk))
	}

	atts := imgs.Filter("unknown/unknown")
	require.Equal(t, len(ps), len(atts.Images))
	for i, att := range atts.Images {
		var attest intoto.Statement
		require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))

		require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
		require.Equal(t, "https://example.com/attestations/v1.0", attest.PredicateType)
		require.Equal(t, map[string]any{"success": true}, attest.Predicate)

		name := fmt.Sprintf("pkg:docker/%s/buildkit/testattestationsemptysubject@latest?platform=%s", url.QueryEscape(registry), url.QueryEscape(platforms.Format(ps[i])))
		subjects := []intoto.Subject{{
			Name: name,
			Digest: map[string]string{
				"sha256": bases[i].Desc.Digest.Encoded(),
			},
		}}
		require.Equal(t, subjects, attest.Subject)
	}
}

func testAttestationBundle(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	ps := []ocispecs.Platform{
		platforms.MustParse("linux/amd64"),
	}

	scratch := func() llb.State {
		return integration.UnixOrWindows(llb.Scratch(), llb.Image("nanoserver"))
	}

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		expPlatforms := &exptypes.Platforms{}

		for _, p := range ps {
			pk := platforms.Format(p)
			expPlatforms.Platforms = append(expPlatforms.Platforms, exptypes.Platform{ID: pk, Platform: p})

			// build image
			st := scratch().File(
				llb.Mkfile("/greeting", 0600, fmt.Appendf(nil, "hello %s!", pk)),
			)
			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(pk, ref)

			stmt := intoto.Statement{
				StatementHeader: intoto.StatementHeader{
					Type:          intoto.StatementInTotoV01,
					PredicateType: "https://example.com/attestations/v1.0",
				},
				Predicate: map[string]any{
					"foo": "1",
				},
			}
			buff := bytes.NewBuffer(nil)
			enc := json.NewEncoder(buff)
			require.NoError(t, enc.Encode(stmt))

			// build attestations
			st = scratch().File(
				llb.Mkdir("/bundle", 0700),
			)
			st = st.File(
				llb.Mkfile("/bundle/attestation.json", 0600, buff.Bytes()),
			)
			def, err = st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err = c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			refAttest, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddAttestation(pk, gateway.Attestation{
				Kind: gatewaypb.AttestationKind_Bundle,
				Ref:  refAttest,
				Path: "/bundle",
			})
		}

		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	target := registry + "/buildkit/testattestationsbundle:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, len(ps)*2, len(imgs.Images))

	var bases []*testutil.ImageInfo
	for _, p := range ps {
		pk := platforms.Format(p)
		bases = append(bases, imgs.Find(pk))
	}

	atts := imgs.Filter("unknown/unknown")
	require.Equal(t, len(ps)*1, len(atts.Images))
	for i, att := range atts.Images {
		require.Equal(t, 1, len(att.LayersRaw))
		var attest intoto.Statement
		require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))

		require.Equal(t, "https://example.com/attestations/v1.0", attest.PredicateType)
		require.Equal(t, map[string]any{"foo": "1"}, attest.Predicate)
		name := fmt.Sprintf("pkg:docker/%s/buildkit/testattestationsbundle@latest?platform=%s", url.QueryEscape(registry), url.QueryEscape(platforms.Format(ps[i])))
		subjects := []intoto.Subject{{
			Name: name,
			Digest: map[string]string{
				"sha256": bases[i].Desc.Digest.Encoded(),
			},
		}}
		require.Equal(t, subjects, attest.Subject)
	}
}

func testSBOMScan(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureSBOM)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	p := platforms.MustParse("linux/amd64")
	pk := platforms.Format(p)

	scannerFrontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()

		st := llb.Image("busybox")
		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)

		r, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ToState()
		if err != nil {
			return nil, err
		}
		res.AddRef(pk, ref)

		expPlatforms := &exptypes.Platforms{
			Platforms: []exptypes.Platform{{ID: pk, Platform: p}},
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		var img ocispecs.Image
		cmd := `
cat <<EOF > $BUILDKIT_SCAN_DESTINATION/spdx.json
{
  "_type": "https://in-toto.io/Statement/v0.1",
  "predicateType": "https://spdx.dev/Document",
  "predicate": {
	"name": "fallback",
	"extraParams": {
	  "ARG1": "$BUILDKIT_SCAN_ARG1",
	  "ARG2": "$BUILDKIT_SCAN_ARG2"
	}
  }
}
EOF
`
		img.Config.Cmd = []string{"/bin/sh", "-c", cmd}
		img.Platform = p
		config, err := json.Marshal(img)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal image config")
		}
		res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, pk), config)

		return res, nil
	}

	scannerTarget := registry + "/buildkit/testsbomscanner:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": scannerTarget,
					"push": "true",
				},
			},
		},
	}, "", scannerFrontend, nil)
	require.NoError(t, err)

	makeTargetFrontend := func(attest bool) func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		return func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
			res := gateway.NewResult()

			// build image
			st := llb.Scratch().File(
				llb.Mkfile("/greeting", 0600, []byte("hello world!")),
			)
			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}
			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(pk, ref)

			expPlatforms := &exptypes.Platforms{
				Platforms: []exptypes.Platform{{ID: pk, Platform: p}},
			}
			dt, err := json.Marshal(expPlatforms)
			if err != nil {
				return nil, err
			}
			res.AddMeta(exptypes.ExporterPlatformsKey, dt)

			// build attestations
			if attest {
				st = llb.Scratch().
					File(llb.Mkfile("/result.spdx", 0600, []byte(`{"name": "frontend"}`)))
				def, err = st.Marshal(ctx)
				if err != nil {
					return nil, err
				}
				r, err = c.Solve(ctx, gateway.SolveRequest{
					Definition: def.ToPB(),
				})
				if err != nil {
					return nil, err
				}
				refAttest, err := r.SingleRef()
				if err != nil {
					return nil, err
				}
				_, err = ref.ToState()
				if err != nil {
					return nil, err
				}

				res.AddAttestation(pk, gateway.Attestation{
					Kind: gatewaypb.AttestationKind_InToto,
					Ref:  refAttest,
					Path: "/result.spdx",
					InToto: result.InTotoAttestation{
						PredicateType: intoto.PredicateSPDX,
					},
				})
			}

			return res, nil
		}
	}

	// test the default fallback scanner
	target := registry + "/buildkit/testsbom:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "",
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(false), nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	// test the frontend builtin scanner
	target = registry + "/buildkit/testsbom2:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "",
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(true), nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att := imgs.Find("unknown/unknown")
	attest := intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{"name": "frontend"})

	// test the specified fallback scanner
	target = registry + "/buildkit/testsbom3:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "generator=" + scannerTarget,
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(false), nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att = imgs.Find("unknown/unknown")
	attest = intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{"name": "fallback"})

	// test the builtin frontend scanner and the specified fallback scanner together
	target = registry + "/buildkit/testsbom3:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "generator=" + scannerTarget,
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(true), nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att = imgs.Find("unknown/unknown")
	attest = intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{"name": "frontend"})

	// test configuring the scanner (simple)
	target = registry + "/buildkit/testsbom4:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "generator=" + scannerTarget + ",ARG1=foo,ARG2=bar",
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(false), nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att = imgs.Find("unknown/unknown")
	attest = intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{
		"extraParams": map[string]any{"ARG1": "foo", "ARG2": "bar"},
	})

	// test configuring the scanner (complex)
	target = registry + "/buildkit/testsbom4:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "\"generator=" + scannerTarget + "\",\"ARG1=foo\",\"ARG2=hello,world\"",
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", makeTargetFrontend(false), nil)
	require.NoError(t, err)

	desc, provider, err = contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err = testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att = imgs.Find("unknown/unknown")
	attest = intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{
		"extraParams": map[string]any{"ARG1": "foo", "ARG2": "hello,world"},
	})
}

func testSBOMScanSingleRef(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureSBOM)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	p := platforms.DefaultSpec()
	pk := platforms.Format(p)

	scannerFrontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()

		st := llb.Image("busybox")
		def, err := st.Marshal(sb.Context())
		require.NoError(t, err)

		r, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ToState()
		if err != nil {
			return nil, err
		}
		res.AddRef(pk, ref)

		expPlatforms := &exptypes.Platforms{
			Platforms: []exptypes.Platform{{ID: pk, Platform: p}},
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		var img ocispecs.Image
		cmd := `
cat <<EOF > $BUILDKIT_SCAN_DESTINATION/spdx.json
{
  "_type": "https://in-toto.io/Statement/v0.1",
  "predicateType": "https://spdx.dev/Document",
  "predicate": {"name": "fallback"}
}
EOF
`
		img.Config.Cmd = []string{"/bin/sh", "-c", cmd}
		img.Platform = p
		config, err := json.Marshal(img)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal image config")
		}
		res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, pk), config)

		return res, nil
	}

	scannerTarget := registry + "/buildkit/testsbomscanner:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": scannerTarget,
					"push": "true",
				},
			},
		},
	}, "", scannerFrontend, nil)
	require.NoError(t, err)

	targetFrontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()

		// build image
		st := llb.Scratch().File(
			llb.Mkfile("/greeting", 0600, []byte("hello world!")),
		)
		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ToState()
		if err != nil {
			return nil, err
		}
		res.SetRef(ref)

		var img ocispecs.Image
		img.Config.Cmd = []string{"/bin/sh", "-c", "cat /greeting"}
		img.Platform = p
		config, err := json.Marshal(img)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal image config")
		}
		res.AddMeta(exptypes.ExporterImageConfigKey, config)

		expPlatforms := &exptypes.Platforms{
			Platforms: []exptypes.Platform{{ID: pk, Platform: p}},
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	target := registry + "/buildkit/testsbomsingle:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "generator=" + scannerTarget,
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", targetFrontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	img := imgs.Find(pk)
	require.NotNil(t, img)
	require.Equal(t, []string{"/bin/sh", "-c", "cat /greeting"}, img.Img.Config.Cmd)

	att := imgs.Find("unknown/unknown")
	require.NotNil(t, att)
	attest := intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)
	require.Subset(t, attest.Predicate, map[string]any{"name": "fallback"})
}

func testSBOMSupplements(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureSBOM)
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	p := platforms.MustParse("linux/amd64")
	pk := platforms.Format(p)

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()

		// build image
		st := llb.Scratch().File(
			llb.Mkfile("/foo", 0600, []byte{}),
		)
		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ToState()
		if err != nil {
			return nil, err
		}
		res.AddRef(pk, ref)

		expPlatforms := &exptypes.Platforms{
			Platforms: []exptypes.Platform{{ID: pk, Platform: p}},
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		// build attestations
		doc := spdx.Document{
			SPDXVersion:    "SPDX-2.2",
			SPDXIdentifier: "DOCUMENT",
			Files: []*spdx.File{
				{
					// foo exists...
					FileSPDXIdentifier: "SPDXRef-File-foo",
					FileName:           "/foo",
				},
				{
					// ...but bar doesn't
					FileSPDXIdentifier: "SPDXRef-File-bar",
					FileName:           "/bar",
				},
			},
		}
		docBytes, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		st = llb.Scratch().
			File(llb.Mkfile("/result.spdx", 0600, docBytes))
		def, err = st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		r, err = c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		refAttest, err := r.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ToState()
		if err != nil {
			return nil, err
		}

		res.AddAttestation(pk, gateway.Attestation{
			Kind: gatewaypb.AttestationKind_InToto,
			Ref:  refAttest,
			Path: "/result.spdx",
			InToto: result.InTotoAttestation{
				PredicateType: intoto.PredicateSPDX,
			},
			Metadata: map[string][]byte{
				result.AttestationSBOMCore: []byte("result"),
			},
		})

		return res, nil
	}

	// test the default fallback scanner
	target := registry + "/buildkit/testsbom:latest"
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:sbom": "",
		},
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)

	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att := imgs.Find("unknown/unknown")
	attest := struct {
		intoto.StatementHeader
		Predicate spdx.Document
	}{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, intoto.PredicateSPDX, attest.PredicateType)

	require.Equal(t, "DOCUMENT", string(attest.Predicate.SPDXIdentifier))
	require.Len(t, attest.Predicate.Files, 2)
	require.Equal(t, "/foo", attest.Predicate.Files[0].FileName)
	require.Regexp(t, "^layerID: sha256:", attest.Predicate.Files[0].FileComment)
	require.Equal(t, "/bar", attest.Predicate.Files[1].FileName)
	require.Empty(t, attest.Predicate.Files[1].FileComment)

	checkAllReleasable(t, c, sb, true)
}

func testMultipleCacheExports(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureMultiCacheExport)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()
	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}
	run(`sh -c "echo -n foobar > const"`)
	run(`sh -c "cat /dev/urandom | head -c 100 | sha256sum > unique"`)

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	target := path.Join(registry, "image:test")
	target2 := path.Join(registry, "image-copy:test")
	cacheRef := path.Join(registry, "cache:test")
	cacheOutDir, cacheOutDir2 := t.TempDir(), t.TempDir()

	res, err := c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "local",
				Attrs: map[string]string{
					"dest": cacheOutDir,
				},
			},
			{
				Type: "local",
				Attrs: map[string]string{
					"dest": cacheOutDir2,
				},
			},
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": cacheRef,
				},
			},
			{
				Type: "inline",
			},
		},
	}, nil)
	require.NoError(t, err)

	ensureFile(t, filepath.Join(cacheOutDir, ocispecs.ImageIndexFile))
	ensureFile(t, filepath.Join(cacheOutDir2, ocispecs.ImageIndexFile))

	dgst := res.ExporterResponse[exptypes.ExporterImageDigestKey]

	uniqueFile, err := readFileInImage(sb.Context(), t, c, target+"@"+dgst, "/unique")
	require.NoError(t, err)

	res, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type: ExporterImage,
				Attrs: map[string]string{
					"name": target2,
					"push": "true",
				},
			},
		},
		CacheExports: []CacheOptionsEntry{
			{
				Type: "inline",
			},
		},
	}, nil)
	require.NoError(t, err)

	dgst2 := res.ExporterResponse[exptypes.ExporterImageDigestKey]
	require.Equal(t, dgst, dgst2)

	destDir := t.TempDir()
	ensurePruneAll(t, c, sb)
	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:      ExporterLocal,
				OutputDir: destDir,
			},
		},
		CacheImports: []CacheOptionsEntry{
			{
				Type: "registry",
				Attrs: map[string]string{
					"ref": cacheRef,
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	ensureFileContents(t, filepath.Join(destDir, "const"), "foobar")
	ensureFileContents(t, filepath.Join(destDir, "unique"), string(uniqueFile))
}

func testMountStubsDirectory(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").
		File(llb.Mkdir("/test", 0700)).
		File(llb.Mkdir("/test/qux/", 0700)).
		Run(
			llb.Args([]string{"touch", "/test/baz/keep"}),
			// check stubs directory is removed
			llb.AddMount("/test/foo", llb.Scratch(), llb.Tmpfs()),
			// check that stubs directory are recursively removed
			llb.AddMount("/test/bar/x/y", llb.Scratch(), llb.Tmpfs()),
			// check that only empty stubs directories are removed
			llb.AddMount("/test/baz/x", llb.Scratch(), llb.Tmpfs()),
			// check that previously existing directory are not removed
			llb.AddMount("/test/qux", llb.Scratch(), llb.Tmpfs()),
		).Root()
	st = llb.Scratch().File(llb.Copy(st, "/test", "/", &llb.CopyInfo{CopyDirContentsOnly: true}))
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	tmpDir := t.TempDir()
	tarFile := filepath.Join(tmpDir, "out.tar")
	tarFileW, err := os.Create(tarFile)
	require.NoError(t, err)
	defer tarFileW.Close()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(tarFileW),
			},
		},
	}, nil)
	require.NoError(t, err)
	tarFileW.Close()

	dt, err := os.ReadFile(tarFile)
	require.NoError(t, err)

	m, err := testutil.ReadTarToMap(dt, false)
	require.NoError(t, err)

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	require.ElementsMatch(t, []string{
		"baz/",
		"baz/keep",
		"qux/",
	}, keys)
}

// https://github.com/moby/buildkit/issues/3148
func testMountStubsTimestamp(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	const sourceDateEpoch = int64(1234567890) // Fri Feb 13 11:31:30 PM UTC 2009
	st := llb.Image("busybox:latest").Run(
		llb.Args([]string{
			"/bin/touch", fmt.Sprintf("--date=@%d", sourceDateEpoch),
			"/bin",
			"/etc",
			"/var",
			"/var/foo",
			"/tmp",
			"/tmp/foo2",
			"/tmp/foo2/bar",
		}),
		llb.AddMount("/var/foo", llb.Scratch(), llb.Tmpfs()),
		llb.AddMount("/tmp/foo2/bar", llb.Scratch(), llb.Tmpfs()),
	)
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	tmpDir := t.TempDir()
	tarFile := filepath.Join(tmpDir, "out.tar")
	tarFileW, err := os.Create(tarFile)
	require.NoError(t, err)
	defer tarFileW.Close()

	_, err = c.Solve(sb.Context(), def, SolveOpt{
		Exports: []ExportEntry{
			{
				Type:   ExporterTar,
				Output: fixedWriteCloser(tarFileW),
			},
		},
	}, nil)
	require.NoError(t, err)
	tarFileW.Close()

	tarFileR, err := os.Open(tarFile)
	require.NoError(t, err)
	defer tarFileR.Close()
	tarR := tar.NewReader(tarFileR)
	touched := map[string]*tar.Header{
		"bin/": nil, // Regular dir
		"etc/": nil, // Parent of file mounts (etc/{resolv.conf, hosts})
		"var/": nil, // Parent of dir mount (var/foo/)
		"tmp/": nil, // Grandparent of dir mount (tmp/foo2/bar/)
		// No support for reproducing the timestamps of mount point directories such as var/foo/ and tmp/foo2/bar/,
		// because the touched timestamp value is lost when the mount is unmounted.
	}
	for {
		hd, err := tarR.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if x, ok := touched[hd.Name]; ok && x == nil {
			touched[hd.Name] = hd
		}
	}
	for name, hd := range touched {
		t.Logf("Verifying %q (%+v)", name, hd)
		require.NotNil(t, hd, name)
		require.Equal(t, sourceDateEpoch, hd.ModTime.Unix(), name)
	}
}

func testFrontendVerifyPlatforms(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.Scratch().File(
			llb.Mkfile("foo", 0600, []byte("data")),
		)

		def, err := st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}

	wc := newWarningsCapture()
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"platform": "linux/amd64,linux/arm64",
		},
	}, "", frontend, wc.status)
	require.NoError(t, err)
	warnings := wc.wait()

	require.Len(t, warnings, 1)
	require.Contains(t, string(warnings[0].Short), "Multiple platforms requested but result is not multi-platform")

	wc = newWarningsCapture()
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{},
	}, "", frontend, wc.status)
	require.NoError(t, err)

	warnings = wc.wait()
	require.Len(t, warnings, 0, warningsListOutput(warnings))

	frontend = func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res := gateway.NewResult()
		platformsToTest := []string{"linux/amd64", "linux/arm64"}
		expPlatforms := &exptypes.Platforms{
			Platforms: make([]exptypes.Platform, len(platformsToTest)),
		}
		for i, platform := range platformsToTest {
			st := llb.Scratch().File(
				llb.Mkfile("platform", 0600, []byte(platform)),
			)

			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}

			r, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}

			ref, err := r.SingleRef()
			if err != nil {
				return nil, err
			}

			_, err = ref.ToState()
			if err != nil {
				return nil, err
			}
			res.AddRef(platform, ref)

			expPlatforms.Platforms[i] = exptypes.Platform{
				ID:       platform,
				Platform: platforms.MustParse(platform),
			}
		}
		dt, err := json.Marshal(expPlatforms)
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)

		return res, nil
	}

	wc = newWarningsCapture()
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"platform": "linux/amd64,linux/arm64",
		},
	}, "", frontend, wc.status)
	require.NoError(t, err)
	warnings = wc.wait()

	require.Len(t, warnings, 0)

	wc = newWarningsCapture()
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{},
	}, "", frontend, wc.status)
	require.NoError(t, err)

	warnings = wc.wait()
	require.Len(t, warnings, 1)
	require.Contains(t, string(warnings[0].Short), "do not match result platforms linux/amd64,linux/arm64")
}

func testFrontendLintSkipVerifyPlatforms(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		dockerfile := `
FROM scratch
COPY foo /foo
    `
		st := llb.Scratch().File(
			llb.Mkfile("Dockerfile", 0600, []byte(dockerfile)).
				Mkfile("foo", 0600, []byte("data")))

		def, err := st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}

		return c.Solve(ctx, gateway.SolveRequest{
			FrontendInputs: map[string]*pb.Definition{
				"context":    def.ToPB(),
				"dockerfile": def.ToPB(),
			},
			FrontendOpt: map[string]string{
				"requestid":     "frontend.lint",
				"frontend.caps": "moby.buildkit.frontend.subrequests",
			},
		})
	}

	wc := newWarningsCapture()
	_, err = c.Build(sb.Context(), SolveOpt{
		FrontendAttrs: map[string]string{
			"platform": "linux/amd64,linux/arm64",
		},
	}, "", frontend, wc.status)
	require.NoError(t, err)
	warnings := wc.wait()

	for _, w := range warnings {
		t.Logf("warning: %s", string(w.Short))
	}
	require.Len(t, warnings, 0)
}

type warningsCapture struct {
	status     chan *SolveStatus
	statusDone chan struct{}
	done       chan struct{}
	warnings   []*VertexWarning
	vertexes   map[digest.Digest]struct{}
}

func newWarningsCapture() *warningsCapture {
	w := &warningsCapture{
		status:     make(chan *SolveStatus),
		statusDone: make(chan struct{}),
		done:       make(chan struct{}),
		vertexes:   map[digest.Digest]struct{}{},
	}

	go func() {
		defer close(w.statusDone)
		for {
			select {
			case st, ok := <-w.status:
				if !ok {
					return
				}
				for _, s := range st.Vertexes {
					w.vertexes[s.Digest] = struct{}{}
				}
				w.warnings = append(w.warnings, st.Warnings...)
			case <-w.done:
				return
			}
		}
	}()

	return w
}

func (w *warningsCapture) wait() []*VertexWarning {
	select {
	case <-w.statusDone:
	case <-time.After(10 * time.Second):
		close(w.done)
	}

	<-w.statusDone
	return w.warnings
}

func ensureFile(t *testing.T, path string) {
	st, err := os.Stat(path)
	require.NoError(t, err, "expected file at %s", path)
	require.True(t, st.Mode().IsRegular())
}

func ensureFileContents(t *testing.T, path, expectedContents string) {
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expectedContents, string(contents))
}

func makeSSHAgentSock(t *testing.T, agent agent.Agent) (p string, err error) {
	tmpDir := integration.Tmpdir(t)
	sockPath := filepath.Join(tmpDir.Name, "ssh_auth_sock")

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return "", err
	}
	t.Cleanup(func() {
		require.NoError(t, l.Close())
	})

	s := &server{l: l}
	go s.run(agent)

	return sockPath, nil
}

type imageTimestamps struct {
	FromImage      []string // from img.Created and img.[]History.Created
	FromAnnotation string   // from index.Manifests[0].Annotations["org.opencontainers.image.created"]
}

func readImageTimestamps(dt []byte) (*imageTimestamps, error) {
	m, err := testutil.ReadTarToMap(dt, false)
	if err != nil {
		return nil, err
	}

	if _, ok := m["oci-layout"]; !ok {
		return nil, errors.Errorf("no oci-layout")
	}

	var index ocispecs.Index
	if err := json.Unmarshal(m[ocispecs.ImageIndexFile].Data, &index); err != nil {
		return nil, err
	}
	if len(index.Manifests) != 1 {
		return nil, errors.Errorf("invalid manifest count %d", len(index.Manifests))
	}

	var res imageTimestamps
	res.FromAnnotation = index.Manifests[0].Annotations[ocispecs.AnnotationCreated]

	var mfst ocispecs.Manifest
	if err := json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+index.Manifests[0].Digest.Hex()].Data, &mfst); err != nil {
		return nil, err
	}
	// don't unmarshal to image type so we get the original string value
	type history struct {
		Created string `json:"created"`
	}

	img := struct {
		History []history `json:"history"`
		Created string    `json:"created"`
	}{}

	if err := json.Unmarshal(m[ocispecs.ImageBlobsDir+"/sha256/"+mfst.Config.Digest.Hex()].Data, &img); err != nil {
		return nil, err
	}

	res.FromImage = []string{
		img.Created,
	}
	for _, h := range img.History {
		res.FromImage = append(res.FromImage, h.Created)
	}
	return &res, nil
}

type server struct {
	l net.Listener
}

func (s *server) run(a agent.Agent) error {
	for {
		c, err := s.l.Accept()
		if err != nil {
			return err
		}

		go agent.ServeAgent(a, c)
	}
}

type secModeSandbox struct{}

func (*secModeSandbox) UpdateConfigFile(in string) string {
	return in
}

type secModeInsecure struct{}

func (*secModeInsecure) UpdateConfigFile(in string) string {
	return in + "\n\ninsecure-entitlements = [\"security.insecure\"]\n"
}

var (
	securitySandbox  integration.ConfigUpdater = &secModeSandbox{}
	securityInsecure integration.ConfigUpdater = &secModeInsecure{}
)

type netModeHost struct{}

func (*netModeHost) UpdateConfigFile(in string) string {
	return in + "\n\ninsecure-entitlements = [\"network.host\"]\n"
}

type netModeDefault struct{}

func (*netModeDefault) UpdateConfigFile(in string) string {
	return in
}

type netModeBridgeDNS struct{}

func (*netModeBridgeDNS) UpdateConfigFile(in string) string {
	return in + `
# configure bridge networking
[worker.oci]
networkMode = "cni"
cniConfigPath = "/etc/buildkit/dns-cni.conflist"

[worker.containerd]
networkMode = "cni"
cniConfigPath = "/etc/buildkit/dns-cni.conflist"

[dns]
nameservers = ["10.11.0.1"]
`
}

var (
	hostNetwork      integration.ConfigUpdater = &netModeHost{}
	defaultNetwork   integration.ConfigUpdater = &netModeDefault{}
	bridgeDNSNetwork integration.ConfigUpdater = &netModeBridgeDNS{}
)

func fixedWriteCloser(wc io.WriteCloser) filesync.FileOutputFunc {
	return func(map[string]string) (io.WriteCloser, error) {
		return wc, nil
	}
}

func testSourcePolicy(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.Image("busybox:1.34.1-uclibc").File(
			llb.Copy(llb.HTTP("https://raw.githubusercontent.com/moby/buildkit/v0.10.1/README.md"),
				"README.md", "README.md"))
		def, err := st.Marshal(sb.Context())
		if err != nil {
			return nil, err
		}
		return c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
	}

	type testCase struct {
		srcPol      *sourcepolicypb.Policy
		expectedErr string
	}
	testCases := []testCase{
		{
			// Valid
			srcPol: &sourcepolicypb.Policy{
				Rules: []*sourcepolicypb.Rule{
					{
						Action: sourcepolicypb.PolicyAction_CONVERT,
						Selector: &sourcepolicypb.Selector{
							Identifier: "docker-image://docker.io/library/busybox:1.34.1-uclibc",
						},
						Updates: &sourcepolicypb.Update{
							Identifier: "docker-image://docker.io/library/busybox:1.34.1-uclibc@sha256:3614ca5eacf0a3a1bcc361c939202a974b4902b9334ff36eb29ffe9011aaad83",
						},
					},
					{
						Action: sourcepolicypb.PolicyAction_CONVERT,
						Selector: &sourcepolicypb.Selector{
							Identifier: "https://raw.githubusercontent.com/moby/buildkit/v0.10.1/README.md",
						},
						Updates: &sourcepolicypb.Update{
							Identifier: "https://raw.githubusercontent.com/moby/buildkit/v0.10.1/README.md",
							Attrs:      map[string]string{"http.checksum": "sha256:6e4b94fc270e708e1068be28bd3551dc6917a4fc5a61293d51bb36e6b75c4b53"},
						},
					},
				},
			},
			expectedErr: "",
		},
		{
			// Invalid docker-image source
			srcPol: &sourcepolicypb.Policy{
				Rules: []*sourcepolicypb.Rule{
					{
						Action: sourcepolicypb.PolicyAction_CONVERT,
						Selector: &sourcepolicypb.Selector{
							Identifier: "docker-image://docker.io/library/busybox:1.34.1-uclibc",
						},
						Updates: &sourcepolicypb.Update{
							Identifier: "docker-image://docker.io/library/busybox:1.34.1-uclibc@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // invalid
						},
					},
				},
			},
			expectedErr: "docker.io/library/busybox:1.34.1-uclibc@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa: not found",
		},
		{
			// Invalid http source
			srcPol: &sourcepolicypb.Policy{
				Rules: []*sourcepolicypb.Rule{
					{
						Action: sourcepolicypb.PolicyAction_CONVERT,
						Selector: &sourcepolicypb.Selector{
							Identifier: "https://raw.githubusercontent.com/moby/buildkit/v0.10.1/README.md",
						},
						Updates: &sourcepolicypb.Update{
							Attrs: map[string]string{pb.AttrHTTPChecksum: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, // invalid
						},
					},
				},
			},
			expectedErr: "digest mismatch sha256:6e4b94fc270e708e1068be28bd3551dc6917a4fc5a61293d51bb36e6b75c4b53: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			_, err = c.Build(sb.Context(), SolveOpt{SourcePolicy: tc.srcPol}, "", frontend, nil)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedErr)
			}
		})
	}

	t.Run("Frontend policies", func(t *testing.T) {
		t.Run("deny http", func(t *testing.T) {
			denied := "https://raw.githubusercontent.com/moby/buildkit/v0.10.1/README.md"
			frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
				st := llb.Image("busybox:1.34.1-uclibc").File(
					llb.Copy(llb.HTTP(denied),
						"README.md", "README.md"))
				def, err := st.Marshal(sb.Context())
				if err != nil {
					return nil, err
				}
				return c.Solve(ctx, gateway.SolveRequest{
					Definition: def.ToPB(),
					SourcePolicies: []*sourcepolicypb.Policy{{
						Rules: []*sourcepolicypb.Rule{
							{
								Action: sourcepolicypb.PolicyAction_DENY,
								Selector: &sourcepolicypb.Selector{
									Identifier: denied,
								},
							},
						},
					}},
				})
			}

			_, err = c.Build(sb.Context(), SolveOpt{}, "", frontend, nil)
			require.ErrorContains(t, err, sourcepolicy.ErrSourceDenied.Error())
		})
		t.Run("resolve image config", func(t *testing.T) {
			frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
				const (
					origRef    = "docker.io/library/busybox:1.34.1-uclibc"
					updatedRef = "docker.io/library/busybox:latest"
				)
				pol := []*sourcepolicypb.Policy{
					{
						Rules: []*sourcepolicypb.Rule{
							{
								Action: sourcepolicypb.PolicyAction_DENY,
								Selector: &sourcepolicypb.Selector{
									Identifier: "*",
								},
							},
							{
								Action: sourcepolicypb.PolicyAction_ALLOW,
								Selector: &sourcepolicypb.Selector{
									Identifier: "docker-image://" + updatedRef + "*",
								},
							},
							{
								Action: sourcepolicypb.PolicyAction_CONVERT,
								Selector: &sourcepolicypb.Selector{
									Identifier: "docker-image://" + origRef,
								},
								Updates: &sourcepolicypb.Update{
									Identifier: "docker-image://" + updatedRef,
								},
							},
						},
					},
				}

				ref, dgst, _, err := c.ResolveImageConfig(ctx, origRef, sourceresolver.Opt{
					SourcePolicies: pol,
				})
				if err != nil {
					return nil, err
				}
				require.Equal(t, updatedRef, ref)
				st := llb.Image(ref + "@" + dgst.String())
				def, err := st.Marshal(sb.Context())
				if err != nil {
					return nil, err
				}
				return c.Solve(ctx, gateway.SolveRequest{
					Definition:     def.ToPB(),
					SourcePolicies: pol,
				})
			}
			_, err = c.Build(sb.Context(), SolveOpt{}, "", frontend, nil)
			require.NoError(t, err)
		})
	})
}

func testLLBMountPerformance(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	srcPath := integration.UnixOrWindows("/bin", "/Windows")

	mntInput := llb.Image(imgName)
	st := llb.Image(imgName)
	var mnts []llb.State
	for range 20 {
		execSt := st.Run(
			llb.Args(integration.UnixOrWindows(
				[]string{"true"},
				[]string{"cmd", "/C", "exit 0"},
			)),
		)
		mnts = append(mnts, mntInput)
		for j := range mnts {
			mnts[j] = execSt.AddMount(fmt.Sprintf("/tmp/bin%d", j), mnts[j], llb.SourcePath(srcPath))
		}
		st = execSt.Root()
	}

	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)

	// Windows images take longer time
	timeout := integration.UnixOrWindows(time.Minute, 3*time.Minute)
	timeoutCtx, cancel := context.WithTimeoutCause(sb.Context(), timeout, nil)
	defer cancel()
	_, err = c.Solve(timeoutCtx, def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testLayerLimitOnMounts(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")

	ctx := sb.Context()

	c, err := New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	base := llb.Image("busybox:latest")

	const numLayers = 110

	for range numLayers {
		base = base.Run(llb.Shlex("sh -c 'echo hello >> /hello'")).Root()
	}

	def, err := base.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(ctx, def, SolveOpt{}, nil)
	require.NoError(t, err)

	ls := llb.Image("busybox:latest").
		Run(llb.Shlexf("ls -l /base/hello"))
	ls.AddMount("/base", base, llb.Readonly)

	def, err = ls.Marshal(sb.Context())
	require.NoError(t, err)

	_, err = c.Solve(ctx, def, SolveOpt{}, nil)
	require.NoError(t, err)
}

func testClientCustomGRPCOpts(t *testing.T, sb integration.Sandbox) {
	var interceptedMethods []string
	intercept := func(
		ctx context.Context,
		method string,
		req,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		interceptedMethods = append(interceptedMethods, method)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	c, err := New(sb.Context(), sb.Address(), WithGRPCDialOption(grpc.WithChainUnaryInterceptor(intercept)))
	require.NoError(t, err)
	defer c.Close()

	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	st := llb.Image(imgName)
	def, err := st.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	require.Contains(t, interceptedMethods, "/moby.buildkit.v1.Control/Solve")
}

func testRunValidExitCodes(t *testing.T, sb integration.Sandbox) {
	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	// no exit codes specified, equivalent to [0]
	imgName := integration.UnixOrWindows("busybox:latest", "nanoserver:latest")
	out := llb.Image(imgName)
	shellPrefix := integration.UnixOrWindows("sh -c", "cmd /C")

	out = out.Run(llb.Shlexf(`%s "exit 0"`, shellPrefix)).Root()
	out = out.Run(llb.Shlexf(`%s "exit 1"`, shellPrefix)).Root()
	def, err := out.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "exit code: 1")

	// empty exit codes, equivalent to [0]
	out = llb.Image(imgName)
	out = out.Run(llb.Shlexf(`%s "exit 0"`, shellPrefix), llb.ValidExitCodes()).Root()
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// if we expect non-zero, those non-zero codes should succeed
	out = llb.Image(imgName)
	out = out.Run(llb.Shlexf(`%s "exit 1"`, shellPrefix), llb.ValidExitCodes(1)).Root()
	out = out.Run(llb.Shlexf(`%s "exit 2"`, shellPrefix), llb.ValidExitCodes(2, 3)).Root()
	out = out.Run(llb.Shlexf(`%s "exit 3"`, shellPrefix), llb.ValidExitCodes(2, 3)).Root()
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	// if we expect non-zero, returning zero should fail
	out = llb.Image(imgName)
	out = out.Run(llb.Shlexf(`%s "exit 0"`, shellPrefix), llb.ValidExitCodes(1)).Root()
	def, err = out.Marshal(sb.Context())
	require.NoError(t, err)
	_, err = c.Solve(sb.Context(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "exit code: 0")
}

type warningsListOutput []*VertexWarning

func (w warningsListOutput) String() string {
	if len(w) == 0 {
		return ""
	}
	var b strings.Builder

	for _, warn := range w {
		_, _ = b.Write(warn.Short)
	}
	return b.String()
}

func parseFSMetadata(t *testing.T, dt []byte) []fsutiltypes.Stat {
	var m []fsutiltypes.Stat
	for len(dt) > 0 {
		var s fsutiltypes.Stat
		n := binary.LittleEndian.Uint32(dt[:4])
		dt = dt[4:]
		err := s.Unmarshal(dt[:n])
		require.NoError(t, err)
		m = append(m, *s.CloneVT())
		dt = dt[n:]
	}
	return m
}
