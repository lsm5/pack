package acceptance

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/google/go-cmp/cmp"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpack/pack/docker"
)

var pack string
var dockerCli *docker.Client

func TestPack(t *testing.T) {
	rand.Seed(time.Now().UTC().UnixNano())

	pack = os.Getenv("PACK_PATH")
	if pack == "" {
		packTmpDir, err := ioutil.TempDir("", "pack.acceptance.binary.")
		if err != nil {
			panic(err)
		}
		if txt, err := exec.Command("go", "build", "-o", filepath.Join(packTmpDir, "pack"), "../cmd/pack").CombinedOutput(); err != nil {
			fmt.Println(string(txt))
			panic(err)
		}
		pack = filepath.Join(packTmpDir, "pack")
		defer os.RemoveAll(packTmpDir)
	}

	var err error
	dockerCli, err = docker.New()
	assertNil(t, err)
	assertNil(t, dockerCli.PullImage("registry:2"))
	assertNil(t, dockerCli.PullImage("sclevine/test"))
	assertNil(t, dockerCli.PullImage("packs/samples"))

	spec.Run(t, "pack", testPack, spec.Report(report.Terminal{}))
}

func testPack(t *testing.T, when spec.G, it spec.S) {
	var homeDir string

	it.Before(func() {
		if _, err := os.Stat(pack); os.IsNotExist(err) {
			t.Fatal("No file found at PACK_PATH environment variable:", pack)
		}

		var err error
		homeDir, err = ioutil.TempDir("", "buildpack.pack.build.homedir.")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(homeDir, ".docker"), 0777); err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFile(filepath.Join(homeDir, ".docker", "config.json"), []byte("{}"), 0666); err != nil {
			t.Fatal(err)
		}
	})
	it.After(func() {
		os.RemoveAll(homeDir)
	})

	when("subcommand is invalid", func() {
		it("prints usage", func() {
			cmd := exec.Command(pack, "some-bad-command")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, _ := cmd.CombinedOutput()
			if !strings.Contains(string(output), `unknown command "some-bad-command" for "pack"`) {
				t.Fatal("Failed to print usage", string(output))
			}
			if !strings.Contains(string(output), `Run 'pack --help' for usage.`) {
				t.Fatal("Failed to print usage", string(output))
			}
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack build", func() {
		var sourceCodePath, repo, repoName, containerName, registryContainerName, registryPort string

		it.Before(func() {
			registryContainerName = "test-registry-" + randString(10)
			run(t, exec.Command("docker", "run", "-d", "--rm", "-p", ":5000", "--name", registryContainerName, "registry:2"))
			registryPort = fetchHostPort(t, registryContainerName)

			var err error
			sourceCodePath, err = ioutil.TempDir("", "pack.build.node_app.")
			if err != nil {
				t.Fatal(err)
			}
			exec.Command("cp", "-r", "testdata/node_app/.", sourceCodePath).Run()

			repo = "some-org/" + randString(10)
			repoName = "localhost:" + registryPort + "/" + repo
			containerName = "test-" + randString(10)
		})
		it.After(func() {
			dockerCli.ContainerKill(context.TODO(), containerName, "SIGKILL")
			dockerCli.ContainerKill(context.TODO(), registryContainerName, "SIGKILL")
			dockerCli.ImageRemove(context.TODO(), repoName, dockertypes.ImageRemoveOptions{Force: true, PruneChildren: true})
			if sourceCodePath != "" {
				os.RemoveAll(sourceCodePath)
			}
		})

		when("'--publish' flag is not specified'", func() {
			it("builds and exports an image", func() {
				cmd := exec.Command(pack, "build", repoName, "-p", sourceCodePath)
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				run(t, cmd)

				run(t, exec.Command("docker", "run", "--name="+containerName, "--rm=true", "-d", "-e", "PORT=8080", "-p", ":8080", repoName))
				launchPort := fetchHostPort(t, containerName)

				time.Sleep(5 * time.Second)
				assertEq(t, fetch(t, "http://localhost:"+launchPort), "Buildpacks Worked! - 1000:1000")

				t.Log("Checking that registry is empty")
				contents := fetch(t, fmt.Sprintf("http://localhost:%s/v2/_catalog", registryPort))
				if strings.Contains(string(contents), repo) {
					t.Fatalf("Should not have published image without the '--publish' flag: got %s", contents)
				}
			})
		}, spec.Parallel(), spec.Report(report.Terminal{}))

		when("'--buildpack' flag is specified", func() {
			javaBpId := "io.buildpacks.samples.java"
			it.Before(func() {
				var err error
				sourceCodePath, err = ioutil.TempDir("", "pack.build.maven_app.")
				if err != nil {
					t.Fatal(err)
				}
				exec.Command("cp", "-r", "testdata/maven_app/.", sourceCodePath).Run()
			})

			// Skip this test for now. The container run at the very end runs java -jar target/testdata-sample-app-1.0-SNAPSHOT.jar
			// instead of java -jar target/testdata-sample-app-1.0-SNAPSHOT-jar-with-dependencies.jar, which ends
			// up exiting immediately
			it.Pend("assumes latest if no version is provided", func() {
				cmd := exec.Command(pack, "build", repoName, "--buildpack", javaBpId, "-p", sourceCodePath)
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				buildOutput := run(t, cmd)

				assertEq(t, strings.Contains(buildOutput, "DETECTING WITH MANUALLY-PROVIDED GROUP:"), true)
				if strings.Contains(buildOutput, "Node.js Buildpack") {
					t.Fatalf("should have skipped Node.js buildpack because --buildpack flag was provided")
				}
				latestInfo := fmt.Sprintf(`No version for '%s' buildpack provided, will use '%s@latest'`, javaBpId, javaBpId)
				if !strings.Contains(buildOutput, latestInfo) {
					t.Fatalf(`expected build output to contain "%s", got "%s"`, latestInfo, buildOutput)
				}
				assertEq(t, strings.Contains(buildOutput, "Sample Java Buildpack: pass"), true)

				run(t, exec.Command("docker", "run", "--name="+containerName, "--rm=true", "-d", "-e", "PORT=8080", "-p", ":8080", repoName))
				launchPort := fetchHostPort(t, containerName)

				time.Sleep(2 * time.Second)
				assertEq(t, fetch(t, "http://localhost:"+launchPort), "Maven buildpack worked!")
			})
		})

		when("'--publish' flag is specified", func() {
			it("builds and exports an image", func() {
				runPackBuild := func() string {
					t.Helper()
					cmd := exec.Command(pack, "build", repoName, "-p", sourceCodePath, "--publish")
					cmd.Env = append(os.Environ(), "HOME="+homeDir)
					return run(t, cmd)
				}
				output := runPackBuild()
				imgSHA, err := imgSHAFromOutput(output, repoName)
				if err != nil {
					fmt.Println(output)
					t.Fatal("Could not determine sha for built image")
				}

				t.Log("Checking that registry has contents")
				contents := fetch(t, fmt.Sprintf("http://localhost:%s/v2/_catalog", registryPort))
				if !strings.Contains(string(contents), repo) {
					t.Fatalf("Expected to see image %s in %s", repo, contents)
				}

				t.Log("run image:", repoName)
				assertNil(t, dockerCli.PullImage(repoName+"@"+imgSHA))
				run(t, exec.Command("docker", "run", "--name="+containerName, "--rm=true", "-d", "-e", "PORT=8080", "-p", ":8080", fmt.Sprintf("%s@%s", repoName, imgSHA)))
				launchPort := fetchHostPort(t, containerName)

				time.Sleep(5 * time.Second)
				assertEq(t, fetch(t, "http://localhost:"+launchPort), "Buildpacks Worked! - 1000:1000")

				t.Log("uses the cache on subsequent run")
				output = runPackBuild()

				regex := regexp.MustCompile(`moved \d+ packages`)
				if !regex.MatchString(output) {
					t.Fatalf("Build failed to use cache: %s", output)
				}
			})
		}, spec.Parallel(), spec.Report(report.Terminal{}))
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when.Pend("pack rebase", func() {
		var repoName, containerName, runBefore, runAfter string
		var buildAndSetRunImage func(runImage, contents1, contents2 string)
		var rootContents1 func() string
		it.Before(func() {
			if err := os.Mkdir(filepath.Join(homeDir, ".pack"), 0777); err != nil {
				t.Fatal(err)
			}

			containerName = "test-" + randString(10)
			repoName = "some-org/" + randString(10)
			runBefore = "run-before/" + randString(10)
			runAfter = "run-after/" + randString(10)

			buildAndSetRunImage = func(runImage, contents1, contents2 string) {
				cmd := exec.Command("docker", "build", "-t", runImage, "-")
				cmd.Stdin = strings.NewReader(fmt.Sprintf("FROM packs/run\nUSER root\nRUN echo %s > /contents1.txt\nRUN echo %s > /contents2.txt\nUSER pack\n", contents1, contents2))
				run(t, cmd)

				assertNil(t, ioutil.WriteFile(filepath.Join(homeDir, ".pack", "config.toml"), []byte(fmt.Sprintf(`
				default-stack-id = "io.buildpacks.stacks.bionic"

				[[stacks]]
				  id = "io.buildpacks.stacks.bionic"
				  build-images = ["packs/build"]
				  run-images = ["%s"]
			`, runImage)), 0666))
			}
			rootContents1 = func() string {
				run(t, exec.Command("docker", "run", "--name="+containerName, "--rm=true", "-d", "-e", "PORT=8080", "-p", ":8080", repoName))
				launchPort := fetchHostPort(t, containerName)
				time.Sleep(5 * time.Second)
				assertEq(t, fetch(t, "http://localhost:"+launchPort), "Buildpacks Worked! - 1000:1000")
				txt := fetch(t, "http://localhost:"+launchPort+"/rootcontents1")
				assertNil(t, dockerCli.ContainerKill(context.TODO(), containerName, "SIGKILL"))
				return txt
			}
		})
		it.After(func() {
			dockerCli.ContainerKill(context.TODO(), containerName, "SIGKILL")
			for _, name := range []string{repoName, runBefore, runAfter} {
				dockerCli.ImageRemove(context.TODO(), name, dockertypes.ImageRemoveOptions{Force: true, PruneChildren: true})
			}
		})

		when("run on daemon", func() {
			it("rebases", func() {
				buildAndSetRunImage(runBefore, "contents-before-1", "contents-before-2")

				cmd := exec.Command(pack, "build", repoName, "-p", "testdata/node_app/", "--no-pull") // , "--publish")
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				run(t, cmd)

				assertEq(t, rootContents1(), "contents-before-1\n")

				buildAndSetRunImage(runAfter, "contents-after-1", "contents-after-2")

				cmd = exec.Command(pack, "rebase", repoName, "--no-pull") // , "--publish")
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				run(t, cmd)

				assertEq(t, rootContents1(), "contents-after-1\n")
			})
		})

		when("run on registry", func() {
			var registryContainerName, registryPort string
			it.Before(func() {
				registryContainerName = "test-registry-" + randString(10)
				run(t, exec.Command("docker", "run", "-d", "--rm", "-p", ":5000", "--name", registryContainerName, "registry:2"))
				registryPort = fetchHostPort(t, registryContainerName)

				repoName = "localhost:" + registryPort + "/" + repoName
				runBefore = "localhost:" + registryPort + "/" + runBefore
				runAfter = "localhost:" + registryPort + "/" + runAfter
			})
			it.After(func() {
				dockerCli.ContainerKill(context.TODO(), registryContainerName, "SIGKILL")
			})
			it("rebases", func() {
				buildAndSetRunImage(runBefore, "contents-before-1", "contents-before-2")
				run(t, exec.Command("docker", "push", runBefore))

				cmd := exec.Command(pack, "build", repoName, "-p", "testdata/node_app/", "--publish")
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				run(t, cmd)

				assertEq(t, rootContents1(), "contents-before-1\n")

				buildAndSetRunImage(runAfter, "contents-after-1", "contents-after-2")
				run(t, exec.Command("docker", "push", runAfter))

				cmd = exec.Command(pack, "rebase", repoName, "--publish")
				cmd.Env = append(os.Environ(), "HOME="+homeDir)
				run(t, cmd)

				assertEq(t, rootContents1(), "contents-after-1\n")
			})
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack create-builder", func() {
		var (
			builderRepoName string
			containerName   string
			repoName        string
		)

		it.Before(func() {
			builderRepoName = "some-org/" + randString(10)
			repoName = "some-org/" + randString(10)
			containerName = "test-" + randString(10)
		})

		it.After(func() {
			dockerCli.ContainerKill(context.TODO(), containerName, "SIGKILL")
			dockerCli.ImageRemove(context.TODO(), builderRepoName, dockertypes.ImageRemoveOptions{Force: true, PruneChildren: true})
		})

		it("builds and exports an image", func() {
			assertNil(t, dockerCli.PullImage("packs/build")) // TODO: control version, 'latest' is not stable across test runs.

			builderTOML := filepath.Join("testdata", "mock_buildpacks", "builder.toml")
			sourceCodePath := filepath.Join("testdata", "mock_app")

			t.Log("create builder image")
			cmd := exec.Command(
				pack, "create-builder",
				builderRepoName,
				"-b", builderTOML,
			)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			run(t, cmd)

			t.Log("build uses order defined in builder.toml")
			cmd = exec.Command(
				pack, "build", repoName,
				"--builder", builderRepoName,
				"--no-pull",
				"--path", sourceCodePath,
			)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			buildOutput := run(t, cmd)
			expectedDetectOutput := "First Mock Buildpack: pass | Second Mock Buildpack: pass | Third Mock Buildpack: pass"
			if !strings.Contains(buildOutput, expectedDetectOutput) {
				t.Fatalf(`Expected build output to contain detection output "%s", got "%s"`, expectedDetectOutput, buildOutput)
			}

			t.Log("run app container")
			cmd = exec.Command("docker", "run", "--name="+containerName, "--rm=true", repoName)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			runOutput := run(t, cmd)
			if !strings.Contains(runOutput, "First Dep Contents") {
				t.Fatalf(`Expected output to contain "First Dep Contents", got "%s"`, runOutput)
			}
			if !strings.Contains(runOutput, "Second Dep Contents") {
				t.Fatalf(`Expected output to contain "First Dep Contents", got "%s"`, runOutput)
			}
			if !strings.Contains(runOutput, "Third Dep Contents") {
				t.Fatalf(`Expected output to contain "Third Dep Contents", got "%s"`, runOutput)
			}

			t.Log("build with multiple --buildpack flags")
			cmd = exec.Command(
				pack, "build", repoName,
				"--builder", builderRepoName,
				"--no-pull",
				"--buildpack", "mock.bp.first",
				"--buildpack", "mock.bp.third@0.0.3-mock",
				"--path", sourceCodePath,
			)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			buildOutput = run(t, cmd)
			latestInfo := `No version for 'mock.bp.first' buildpack provided, will use 'mock.bp.first@latest'`
			if !strings.Contains(buildOutput, latestInfo) {
				t.Fatalf(`expected build output to contain "%s", got "%s"`, latestInfo, buildOutput)
			}
			expectedDetectOutput = "Latest First Mock Buildpack: pass | Third Mock Buildpack: pass"
			if !strings.Contains(buildOutput, expectedDetectOutput) {
				t.Fatalf(`Expected build output to contain detection output "%s", got "%s"`, expectedDetectOutput, buildOutput)
			}

			t.Log("run app container")
			cmd = exec.Command("docker", "run", "--name="+containerName, "--rm=true", repoName)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			runOutput = run(t, cmd)
			if !strings.Contains(runOutput, "Latest First Dep Contents") {
				t.Fatalf(`Expected output to contain "First Dep Contents", got "%s"`, runOutput)
			}
			if strings.Contains(runOutput, "Second Dep Contents") {
				t.Fatalf(`Should not have run second buildpack, got "%s"`, runOutput)
			}
			if !strings.Contains(runOutput, "Third Dep Contents") {
				t.Fatalf(`Expected output to contain "Third Dep Contents", got "%s"`, runOutput)
			}
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack add-stack", func() {
		it("adds a custom stack to ~/.pack/config.toml", func() {
			cmd := exec.Command(pack, "add-stack", "my.custom.stack", "--run-image", "my-org/run", "--build-image", "my-org/build")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("add-stack command failed: %s: %s", output, err)
			}

			assertEq(t, string(output), "my.custom.stack successfully added\n")

			var config struct {
				Stacks []struct {
					ID          string   `toml:"id"`
					BuildImages []string `toml:"build-images"`
					RunImages   []string `toml:"run-images"`
				} `toml:"stacks"`
			}
			_, err = toml.DecodeFile(filepath.Join(homeDir, ".pack", "config.toml"), &config)
			assertNil(t, err)

			stack := config.Stacks[len(config.Stacks)-1]
			assertEq(t, stack.ID, "my.custom.stack")
			assertEq(t, stack.BuildImages, []string{"my-org/build"})
			assertEq(t, stack.RunImages, []string{"my-org/run"})
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack update-stack", func() {
		type config struct {
			Stacks []struct {
				ID          string   `toml:"id"`
				BuildImages []string `toml:"build-images"`
				RunImages   []string `toml:"run-images"`
			} `toml:"stacks"`
		}

		it.Before(func() {
			cmd := exec.Command(pack, "add-stack", "my.custom.stack", "--run-image", "my-org/run", "--build-image", "my-org/build")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("add-stack command failed: %s: %s", output, err)
			}
		})

		it("updates an existing custom stack in ~/.pack/config.toml", func() {
			cmd := exec.Command(pack, "update-stack", "my.custom.stack", "--run-image", "my-org/run-2", "--run-image", "my-org/run-3", "--build-image", "my-org/build-2", "--build-image", "my-org/build-3")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("update-stack command failed: %s: %s", output, err)
			}
			assertEq(t, string(output), "my.custom.stack successfully updated\n")

			var config config
			_, err = toml.DecodeFile(filepath.Join(homeDir, ".pack", "config.toml"), &config)
			assertNil(t, err)

			stack := config.Stacks[len(config.Stacks)-1]
			assertEq(t, stack.ID, "my.custom.stack")
			assertEq(t, stack.BuildImages, []string{"my-org/build-2", "my-org/build-3"})
			assertEq(t, stack.RunImages, []string{"my-org/run-2", "my-org/run-3"})
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack update-stack", func() {
		type config struct {
			DefaultStackID string `toml:"default-stack-id"`
		}

		it.Before(func() {
			cmd := exec.Command(pack, "add-stack", "my.custom.stack", "--run-image", "my-org/run", "--build-image", "my-org/build")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("add-stack command failed: %s: %s", output, err)
			}
		})

		it("sets the default-stack-id in ~/.pack/config.toml", func() {
			cmd := exec.Command(
				pack,
				"set-default-stack",
				"my.custom.stack",
			)
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("set-default-stack command failed: %s: %s", output, err)
			}
			assertEq(t, string(output), "my.custom.stack is now the default stack\n")

			var config config
			_, err = toml.DecodeFile(filepath.Join(homeDir, ".pack", "config.toml"), &config)
			assertNil(t, err)
			assertEq(t, config.DefaultStackID, "my.custom.stack")
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))

	when("pack delete-stack", func() {
		type config struct {
			Stacks []struct {
				ID          string   `toml:"id"`
				BuildImages []string `toml:"build-images"`
				RunImages   []string `toml:"run-images"`
			} `toml:"stacks"`
		}
		containsStack := func(c config, stackID string) bool {
			for _, s := range c.Stacks {
				if s.ID == stackID {
					return true
				}
			}
			return false
		}

		it.Before(func() {
			cmd := exec.Command(pack, "add-stack", "my.custom.stack", "--run-image", "my-org/run", "--build-image", "my-org/build")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("add-stack command failed: %s: %s", output, err)
			}
		})

		it("deletes a custom stack from ~/.pack/config.toml", func() {
			var config config
			_, err := toml.DecodeFile(filepath.Join(homeDir, ".pack", "config.toml"), &config)
			assertNil(t, err)
			numStacks := len(config.Stacks)
			assertEq(t, containsStack(config, "my.custom.stack"), true)

			cmd := exec.Command(pack, "delete-stack", "my.custom.stack")
			cmd.Env = append(os.Environ(), "HOME="+homeDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("add-stack command failed: %s: %s", output, err)
			}
			assertEq(t, string(output), "my.custom.stack has been successfully deleted\n")

			_, err = toml.DecodeFile(filepath.Join(homeDir, ".pack", "config.toml"), &config)
			assertNil(t, err)
			assertEq(t, len(config.Stacks), numStacks-1)
			assertEq(t, containsStack(config, "my.custom.stack"), false)
		})
	}, spec.Parallel(), spec.Report(report.Terminal{}))
}

func run(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to execute command: %v, %s, %s", cmd.Args, err, output)
	}

	return string(output)
}

func fetch(t *testing.T, url string) string {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Failed to make request to [%s]: %s", url, err)
	}

	contents, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to make request to [%s]: %s", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Request returned bad status code: [%d] : %s", resp.StatusCode, contents)
	}

	return string(contents)
}

func fetchHostPort(t *testing.T, dockerID string) string {
	t.Helper()

	i, err := dockerCli.ContainerInspect(context.Background(), dockerID)
	assertNil(t, err)
	for _, port := range i.NetworkSettings.Ports {
		for _, binding := range port {
			return binding.HostPort
		}
	}

	t.Fatalf("Failed to fetch host port for %s: no ports exposed", dockerID)
	return ""
}

func imgSHAFromOutput(txt, repoName string) (string, error) {
	for _, m := range regexp.MustCompile(`\*\*\* Image: (.+)@(.+)`).FindAllStringSubmatch(txt, -1) {
		if m[1] == repoName {
			return m[2], nil
		}
	}
	return "", fmt.Errorf("could not find Image: %s@[SHA] in output", repoName)
}

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(rand.Intn(26))
	}
	return string(b)
}

func assertEq(t *testing.T, actual, expected interface{}) {
	t.Helper()
	if diff := cmp.Diff(actual, expected); diff != "" {
		t.Fatal(diff)
	}
}

func assertNil(t *testing.T, actual interface{}) {
	t.Helper()
	if actual != nil {
		t.Fatalf("Expected nil: %s", actual)
	}
}

func contains(arr []string, val string) bool {
	for _, v := range arr {
		if v == val {
			return true
		}
	}
	return false
}
