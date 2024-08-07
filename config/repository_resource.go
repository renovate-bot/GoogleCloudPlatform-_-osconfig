//  Copyright 2020 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/osconfig/agentconfig"
	"github.com/GoogleCloudPlatform/osconfig/clog"
	"github.com/GoogleCloudPlatform/osconfig/packages"
	"github.com/GoogleCloudPlatform/osconfig/util"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"

	"cloud.google.com/go/osconfig/agentendpoint/apiv1/agentendpointpb"
)

const aptGPGDir = "/etc/apt/trusted.gpg.d"

type repositoryResource struct {
	*agentendpointpb.OSPolicy_Resource_RepositoryResource

	managedRepository ManagedRepository
}

// AptRepository describes an apt repository resource.
type AptRepository struct {
	RepositoryResource *agentendpointpb.OSPolicy_Resource_RepositoryResource_AptRepository
	GpgFilePath        string
	GpgChecksum        string
	GpgFileContents    []byte
}

// GooGetRepository describes an googet repository resource.
type GooGetRepository struct {
	RepositoryResource *agentendpointpb.OSPolicy_Resource_RepositoryResource_GooRepository
}

// YumRepository describes an yum repository resource.
type YumRepository struct {
	RepositoryResource *agentendpointpb.OSPolicy_Resource_RepositoryResource_YumRepository
}

// ZypperRepository describes an zypper repository resource.
type ZypperRepository struct {
	RepositoryResource *agentendpointpb.OSPolicy_Resource_RepositoryResource_ZypperRepository
}

// ManagedRepository is the repository that this RepositoryResource manages.
type ManagedRepository struct {
	Apt              *AptRepository
	GooGet           *GooGetRepository
	Yum              *YumRepository
	Zypper           *ZypperRepository
	RepoFilePath     string
	RepoChecksum     string
	RepoFileContents []byte
}

func aptRepoContents(repo *agentendpointpb.OSPolicy_Resource_RepositoryResource_AptRepository) []byte {
	var debArchiveTypeMap = map[agentendpointpb.OSPolicy_Resource_RepositoryResource_AptRepository_ArchiveType]string{
		agentendpointpb.OSPolicy_Resource_RepositoryResource_AptRepository_DEB:     "deb",
		agentendpointpb.OSPolicy_Resource_RepositoryResource_AptRepository_DEB_SRC: "deb-src",
	}

	/*
		# Repo file managed by Google OSConfig agent
		deb http://repo1-url/ repo main
	*/
	var buf bytes.Buffer
	buf.WriteString("# Repo file managed by Google OSConfig agent\n")
	archiveType, ok := debArchiveTypeMap[repo.GetArchiveType()]
	if !ok {
		archiveType = "deb"
	}
	line := fmt.Sprintf("%s %s %s", archiveType, repo.GetUri(), repo.GetDistribution())
	for _, c := range repo.GetComponents() {
		line = fmt.Sprintf("%s %s", line, c)
	}
	buf.WriteString(line + "\n")

	return buf.Bytes()
}

func googetRepoContents(repo *agentendpointpb.OSPolicy_Resource_RepositoryResource_GooRepository) []byte {
	/*
		# Repo file managed by Google OSConfig agent
		- name: repo1-name
		  url: https://repo1-url
	*/
	var buf bytes.Buffer
	buf.WriteString("# Repo file managed by Google OSConfig agent\n")
	buf.WriteString(fmt.Sprintf("- name: %s\n", repo.Name))
	buf.WriteString(fmt.Sprintf("  url: %s\n", repo.Url))

	return buf.Bytes()
}

func yumRepoContents(repo *agentendpointpb.OSPolicy_Resource_RepositoryResource_YumRepository) []byte {
	/*
		# Repo file managed by Google OSConfig agent
		[Id]
		name=DisplayName
		baseurl=https://repo-url
		enabled=1
		gpgcheck=1
		gpgkey=http://repo-url/gpg1
		       http://repo-url/gpg2
	*/
	var buf bytes.Buffer
	buf.WriteString("# Repo file managed by Google OSConfig agent\n")
	buf.WriteString(fmt.Sprintf("[%s]\n", repo.Id))
	if repo.DisplayName == "" {
		buf.WriteString(fmt.Sprintf("name=%s\n", repo.Id))
	} else {
		buf.WriteString(fmt.Sprintf("name=%s\n", repo.DisplayName))
	}
	buf.WriteString(fmt.Sprintf("baseurl=%s\n", repo.BaseUrl))
	buf.WriteString("enabled=1\ngpgcheck=1\n")
	if len(repo.GpgKeys) > 0 {
		buf.WriteString(fmt.Sprintf("gpgkey=%s\n", repo.GpgKeys[0]))
		for _, k := range repo.GpgKeys[1:] {
			buf.WriteString(fmt.Sprintf("       %s\n", k))
		}
	}
	return buf.Bytes()
}

func zypperRepoContents(repo *agentendpointpb.OSPolicy_Resource_RepositoryResource_ZypperRepository) []byte {
	/*
		# Repo file managed by Google OSConfig agent
		[Id]
		name=DisplayName
		baseurl=https://repo-url
		enabled=1
		gpgkey=https://repo-url/gpg1
		       https://repo-url/gpg2
	*/
	var buf bytes.Buffer
	buf.WriteString("# Repo file managed by Google OSConfig agent\n")
	buf.WriteString(fmt.Sprintf("[%s]\n", repo.Id))
	if repo.DisplayName == "" {
		buf.WriteString(fmt.Sprintf("name=%s\n", repo.Id))
	} else {
		buf.WriteString(fmt.Sprintf("name=%s\n", repo.DisplayName))
	}
	buf.WriteString(fmt.Sprintf("baseurl=%s\n", repo.BaseUrl))
	buf.WriteString("enabled=1\n")
	if len(repo.GpgKeys) > 0 {
		buf.WriteString(fmt.Sprintf("gpgkey=%s\n", repo.GpgKeys[0]))
		for _, k := range repo.GpgKeys[1:] {
			buf.WriteString(fmt.Sprintf("       %s\n", k))
		}
	}
	return buf.Bytes()
}

func serializeGPGKeyEntity(entityList openpgp.EntityList) ([]byte, error) {
	var buf bytes.Buffer
	for _, entity := range entityList {
		if err := entity.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("error serializing gpg key: %v", err)
		}
	}
	return buf.Bytes(), nil
}

func isArmoredGPGKey(keyData []byte) bool {
	var buf bytes.Buffer
	tee := io.TeeReader(bytes.NewReader(keyData), &buf)

	// Try decoding as armored
	decodedBlock, err := armor.Decode(tee)
	if err == nil && decodedBlock != nil {
		return true
	}

	return false
}

func fetchGPGKey(key string) (openpgp.EntityList, error) {
	resp, err := http.Get(key)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > 1024*1024 {
		return nil, fmt.Errorf("key size of %d too large", resp.ContentLength)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("can not read response body for key %s, err: %v", key, err)
	}

	if isArmoredGPGKey(responseBody) {
		return openpgp.ReadArmoredKeyRing(bytes.NewBuffer(responseBody))
	}

	return openpgp.ReadKeyRing(bytes.NewReader(responseBody))

}

func (r *repositoryResource) validate(ctx context.Context) (*ManagedResources, error) {
	var repoFormat string
	switch r.GetRepository().(type) {
	case *agentendpointpb.OSPolicy_Resource_RepositoryResource_Apt:
		if !packages.AptExists {
			return nil, errors.New("cannot manage Apt repository because apt-get does not exist on the system")
		}
		gpgkey := r.GetApt().GetGpgKey()
		r.managedRepository.Apt = &AptRepository{RepositoryResource: r.GetApt()}
		r.managedRepository.RepoFileContents = aptRepoContents(r.GetApt())
		repoFormat = agentconfig.AptRepoFormat()
		if gpgkey != "" {
			entityList, err := fetchGPGKey(gpgkey)
			if err != nil {
				return nil, fmt.Errorf("error fetching apt gpg key %q: %v", gpgkey, err)
			}
			keyContents, err := serializeGPGKeyEntity(entityList)
			if err != nil {
				return nil, fmt.Errorf("error fetching apt gpg key %q: %v", gpgkey, err)
			}

			r.managedRepository.Apt.GpgFileContents = keyContents
			r.managedRepository.Apt.GpgChecksum = checksum(bytes.NewReader(keyContents))
			r.managedRepository.Apt.GpgFilePath = filepath.Join(aptGPGDir, "osconfig_added_"+r.managedRepository.Apt.GpgChecksum+".gpg")
		}

	case *agentendpointpb.OSPolicy_Resource_RepositoryResource_Goo:
		if !packages.GooGetExists {
			return nil, errors.New("cannot manage googet repository because googet does not exist on the system")
		}
		r.managedRepository.GooGet = &GooGetRepository{RepositoryResource: r.GetGoo()}
		r.managedRepository.RepoFileContents = googetRepoContents(r.GetGoo())
		repoFormat = agentconfig.GooGetRepoFormat()

	case *agentendpointpb.OSPolicy_Resource_RepositoryResource_Yum:
		if !packages.YumExists {
			return nil, errors.New("cannot manage yum repository because yum does not exist on the system")
		}
		r.managedRepository.Yum = &YumRepository{RepositoryResource: r.GetYum()}
		r.managedRepository.RepoFileContents = yumRepoContents(r.GetYum())
		repoFormat = agentconfig.YumRepoFormat()

	case *agentendpointpb.OSPolicy_Resource_RepositoryResource_Zypper:
		if !packages.ZypperExists {
			return nil, errors.New("cannot manage zypper repository because zypper does not exist on the system")
		}
		r.managedRepository.Zypper = &ZypperRepository{RepositoryResource: r.GetZypper()}
		r.managedRepository.RepoFileContents = zypperRepoContents(r.GetZypper())
		repoFormat = agentconfig.ZypperRepoFormat()
	default:
		return nil, fmt.Errorf("Repository field not set or references unknown repository type: %v", r.GetRepository())
	}

	r.managedRepository.RepoChecksum = checksum(bytes.NewReader(r.managedRepository.RepoFileContents))
	r.managedRepository.RepoFilePath = fmt.Sprintf(repoFormat, r.managedRepository.RepoChecksum[:10])
	return &ManagedResources{Repositories: []ManagedRepository{r.managedRepository}}, nil
}

func contentsMatch(ctx context.Context, path, chksum string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			clog.Debugf(ctx, "File not found: %s", path)
			return false, nil
		}
		clog.Debugf(ctx, "Error opening file %s.", path)
		return false, err
	}
	defer file.Close()

	actualChecksum := checksum(file)
	if actualChecksum != chksum {
		clog.Debugf(ctx, "Checksums don't match, got: %s, actual: %s", chksum, actualChecksum)
		return false, nil
	}

	return true, nil
}

func (r *repositoryResource) checkState(ctx context.Context) (inDesiredState bool, err error) {
	// Check APT gpg key if applicable.
	if r.managedRepository.Apt != nil && r.managedRepository.Apt.GpgFileContents != nil {
		match, err := contentsMatch(ctx, r.managedRepository.Apt.GpgFilePath, r.managedRepository.Apt.GpgChecksum)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	return contentsMatch(ctx, r.managedRepository.RepoFilePath, r.managedRepository.RepoChecksum)
}

func (r *repositoryResource) enforceState(ctx context.Context) (inDesiredState bool, err error) {
	clog.Infof(ctx, "Enforcing repo %s.", r.managedRepository.RepoFilePath)
	// Set APT gpg key if applicable.
	if r.managedRepository.Apt != nil && r.managedRepository.Apt.GpgFileContents != nil {
		if err := ioutil.WriteFile(r.managedRepository.Apt.GpgFilePath, r.managedRepository.Apt.GpgFileContents, 0644); err != nil {
			return false, err
		}
	}

	if err := os.MkdirAll(filepath.Dir(r.managedRepository.RepoFilePath), 0755); err != nil {
		return false, err
	}
	if err := util.AtomicWrite(r.managedRepository.RepoFilePath, r.managedRepository.RepoFileContents, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func (r *repositoryResource) populateOutput(rCompliance *agentendpointpb.OSPolicyResourceCompliance) {
}

func (r *repositoryResource) cleanup(ctx context.Context) error {
	return nil
}
