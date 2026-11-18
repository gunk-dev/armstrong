package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	// maxMetadataBlob bounds what is read into memory: the index, manifest
	// and config, never layers.
	maxMetadataBlob = 4 << 20
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// imageInfo is what an OCI archive holds: one image manifest, identified by
// the SHA-256 of its bytes, and the platform its config declares.
type imageInfo struct {
	digest string
	os     string
	arch   string
}

// inspectArchive reads an OCI image layout tar holding exactly one
// single-platform image, verifying the manifest and config blobs against
// their digests. The tar must carry entries for its blob directories, as the
// tars skopeo writes do: unprivileged, skopeo cannot unpack one without them.
func inspectArchive(file string) (imageInfo, error) {
	f, err := os.Open(file)
	if err != nil {
		return imageInfo{}, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return imageInfo{}, errors.New("image output is not a single archive file")
	}

	index, dirs, err := archiveMember(f, "index.json")
	if err != nil {
		return imageInfo{}, err
	}
	if !dirs["blobs"] || !dirs["blobs/sha256"] {
		return imageInfo{}, errors.New("image archive has no directory entries for blobs/sha256")
	}
	var idx struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return imageInfo{}, fmt.Errorf("image index: %w", err)
	}
	if len(idx.Manifests) != 1 {
		return imageInfo{}, fmt.Errorf("image archive holds %d manifests, want exactly one", len(idx.Manifests))
	}
	desc := idx.Manifests[0]
	if desc.MediaType != mediaOCIManifest && desc.MediaType != mediaDockerManifest {
		return imageInfo{}, fmt.Errorf("image archive holds %q, not a single-platform image manifest", desc.MediaType)
	}
	manifest, err := verifiedBlob(f, desc)
	if err != nil {
		return imageInfo{}, fmt.Errorf("image manifest: %w", err)
	}
	var m struct {
		Config descriptor `json:"config"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return imageInfo{}, fmt.Errorf("image manifest: %w", err)
	}
	config, err := verifiedBlob(f, m.Config)
	if err != nil {
		return imageInfo{}, fmt.Errorf("image config: %w", err)
	}
	var c struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	if err := json.Unmarshal(config, &c); err != nil {
		return imageInfo{}, fmt.Errorf("image config: %w", err)
	}
	return imageInfo{digest: desc.Digest, os: c.OS, arch: c.Architecture}, nil
}

// archiveMember scans the tar from the start for the regular file name,
// seeking over every other member, and returns its content along with the
// directories the tar lists. A tar holding name twice is refused: unpacking
// keeps whichever copy comes last, so what was checked might not be what is
// pushed.
func archiveMember(f *os.File, name string) ([]byte, map[string]bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	var data []byte
	dirs := map[string]bool{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read image archive: %w", err)
		}
		member := path.Clean(strings.TrimPrefix(h.Name, "./"))
		switch {
		case h.Typeflag == tar.TypeDir:
			dirs[member] = true
		case member == name && data != nil:
			return nil, nil, fmt.Errorf("image archive holds %s more than once", name)
		case h.Typeflag == tar.TypeReg && member == name:
			if h.Size > maxMetadataBlob {
				return nil, nil, fmt.Errorf("%s is larger than %d bytes", name, maxMetadataBlob)
			}
			if data, err = io.ReadAll(tr); err != nil {
				return nil, nil, fmt.Errorf("read image archive: %w", err)
			}
		}
	}
	if data == nil {
		return nil, nil, fmt.Errorf("image archive has no %s", name)
	}
	return data, dirs, nil
}

func verifiedBlob(f *os.File, d descriptor) ([]byte, error) {
	if !digestRE.MatchString(d.Digest) {
		return nil, fmt.Errorf("unsupported digest %q", d.Digest)
	}
	data, _, err := archiveMember(f, "blobs/sha256/"+strings.TrimPrefix(d.Digest, "sha256:"))
	if err != nil {
		return nil, err
	}
	if "sha256:"+sha256Hex(data) != d.Digest || int64(len(data)) != d.Size {
		return nil, fmt.Errorf("blob %s does not match its descriptor", d.Digest)
	}
	return data, nil
}

// checkPlatform rejects an image built for anything but the packaged system.
func checkPlatform(info imageInfo, system string) error {
	want := platforms[system]
	if info.os != want.os || info.arch != want.arch {
		return fmt.Errorf("image platform %s/%s is incompatible with %s (want %s/%s)", info.os, info.arch, system, want.os, want.arch)
	}
	return nil
}
