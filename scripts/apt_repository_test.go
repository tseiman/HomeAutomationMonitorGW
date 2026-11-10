package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflowPublishesSignedAPTRepository(t *testing.T) {
	root := projectRoot(t)
	workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, expected := range []string{
		"APT_SIGNING_PRIVATE_KEY: ${{ secrets.APT_SIGNING_PRIVATE_KEY }}",
		"./scripts/build_apt_repository.sh release-assets pages-site/apt stable",
		"FBB4E68DABB4A25F6E73C81DD4EA266E8BC8043F",
		"actions/upload-pages-artifact@v4",
		"actions/deploy-pages@v5",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("release workflow lacks %q", expected)
		}
	}
	if strings.Contains(text, "BEGIN PGP PRIVATE KEY BLOCK") {
		t.Fatal("release workflow contains private key material")
	}

	publicKey := filepath.Join(root, "packaging/apt/homeautomationmonitorgw-archive-key.asc")
	output, err := exec.Command("gpg", "--batch", "--show-keys", "--with-colons", publicKey).CombinedOutput()
	if err != nil {
		t.Fatalf("cannot inspect public APT key: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "fpr:::::::::FBB4E68DABB4A25F6E73C81DD4EA266E8BC8043F:") {
		t.Fatalf("unexpected public APT key fingerprint:\n%s", output)
	}
}

func TestBuildAPTRepositoryCreatesIndexedPool(t *testing.T) {
	for _, command := range []string{"dpkg-deb", "dpkg-scanpackages", "apt-ftparchive", "gzip"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is not installed", command)
		}
	}

	root := projectRoot(t)
	packages := t.TempDir()
	for _, name := range []string{"automation-gateway", "automation-gateway-led"} {
		buildTestDebianPackage(t, packages, name, "1.2.3", "arm64")
	}

	repository := filepath.Join(t.TempDir(), "apt")
	command := exec.Command("bash", filepath.Join(root, "scripts/build_apt_repository.sh"), packages, repository, "stable")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build_apt_repository.sh failed: %v\n%s", err, output)
	}

	packagesPath := filepath.Join(repository, "dists/stable/main/binary-arm64/Packages")
	contents, err := os.ReadFile(packagesPath)
	if err != nil {
		t.Fatal(err)
	}
	index := string(contents)
	for _, expected := range []string{
		"Package: automation-gateway\n",
		"Package: automation-gateway-led\n",
		"Version: 1.2.3\n",
		"Filename: pool/main/h/homeautomationmonitorgw/automation-gateway_1.2.3_arm64.deb\n",
	} {
		if !strings.Contains(index, expected) {
			t.Fatalf("Packages index lacks %q:\n%s", expected, index)
		}
	}
	if _, err := os.Stat(packagesPath + ".gz"); err != nil {
		t.Fatal(err)
	}

	release, err := os.ReadFile(filepath.Join(repository, "dists/stable/Release"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Suite: stable\n", "Codename: stable\n", "Architectures: arm64\n", "Components: main\n"} {
		if !strings.Contains(string(release), expected) {
			t.Fatalf("Release lacks %q:\n%s", expected, release)
		}
	}
}

func buildTestDebianPackage(t *testing.T, output, name, version, architecture string) {
	t.Helper()
	stage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stage, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	control := "Package: " + name + "\nVersion: " + version + "\nArchitecture: " + architecture + "\nMaintainer: test <test@example.invalid>\nDescription: test package\n"
	if err := os.WriteFile(filepath.Join(stage, "DEBIAN/control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	payloadDir := filepath.Join(stage, "usr/share", name)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadDir, "payload"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(output, name+"_"+version+"_"+architecture+".deb")
	if combined, err := exec.Command("dpkg-deb", "--root-owner-group", "--build", stage, path).CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb failed: %v\n%s", err, combined)
	}
}
