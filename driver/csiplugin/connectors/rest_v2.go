/**
 * Copyright 2019 IBM Corp.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package connectors

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/ibm-spectrum-scale-csi/driver/csiplugin/settings"
	"github.com/IBM/ibm-spectrum-scale-csi/driver/csiplugin/utils"
	"github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type spectrumRestV2 struct {
	httpClient *http.Client
	endpoint   string
	user       string
	password   string
}

func (s *spectrumRestV2) isStatusOK(statusCode int) bool {
	glog.V(4).Infof("rest_v2 isStatusOK. statusCode: %d", statusCode)

	if (statusCode == http.StatusOK) ||
		(statusCode == http.StatusCreated) ||
		(statusCode == http.StatusAccepted) {
		return true
	}
	return false
}

func (s *spectrumRestV2) checkAsynchronousJob(statusCode int) bool {
	glog.V(4).Infof("rest_v2 checkAsynchronousJob. statusCode: %d", statusCode)
	if (statusCode == http.StatusAccepted) ||
		(statusCode == http.StatusCreated) {
		return true
	}
	return false
}

func (s *spectrumRestV2) isRequestAccepted(response GenericResponse, url string) error {
	glog.V(4).Infof("rest_v2 isRequestAccepted. url: %s, response: %v", url, response)

	if !s.isStatusOK(response.Status.Code) {
		return fmt.Errorf("error %v for url %v", response, url)
	}

	if len(response.Jobs) == 0 {
		return fmt.Errorf("Unable to get Job details for %s request: %v", url, response)
	}
	return nil
}

func (s *spectrumRestV2) waitForJobCompletion(statusCode int, jobID uint64) error {
	glog.V(4).Infof("rest_v2 waitForJobCompletion. jobID: %d, statusCode: %d", jobID, statusCode)

	if s.checkAsynchronousJob(statusCode) {
		jobURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/jobs/%d?fields=:all:", jobID))
		_, err := s.AsyncJobCompletion(jobURL)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *spectrumRestV2) waitForJobCompletionWithResp(statusCode int, jobID uint64) (GenericResponse, error) {
	glog.V(4).Infof("rest_v2 waitForJobCompletionWithResp. jobID: %d, statusCode: %d", jobID, statusCode)

	if s.checkAsynchronousJob(statusCode) {
		response := GenericResponse{}
		jobURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/jobs/%d?fields=:all:", jobID))
		response, err := s.AsyncJobCompletion(jobURL)
		if err != nil {
			return GenericResponse{}, err
		}
		return response, nil
	}
	return GenericResponse{}, nil
}

func (s *spectrumRestV2) AsyncJobCompletion(jobURL string) (GenericResponse, error) {
	glog.V(4).Infof("rest_v2 AsyncJobCompletion. jobURL: %s", jobURL)

	jobQueryResponse := GenericResponse{}
	var waitTime time.Duration = 2
	for {
		err := s.doHTTP(jobURL, "GET", &jobQueryResponse, nil)
		if err != nil {
			return GenericResponse{}, err
		}
		if len(jobQueryResponse.Jobs) == 0 {
			return GenericResponse{}, fmt.Errorf("unable to get Job details for %s: %v", jobURL, jobQueryResponse)
		}

		if jobQueryResponse.Jobs[0].Status == "RUNNING" {
			time.Sleep(waitTime * time.Second)
			if waitTime < 16 {
				waitTime = waitTime * 2
			}
			continue
		}
		break
	}
	if jobQueryResponse.Jobs[0].Status == "COMPLETED" {
		return jobQueryResponse, nil
	} else {
		glog.Errorf("Async Job failed: %v", jobQueryResponse)
		return GenericResponse{}, fmt.Errorf("%v", jobQueryResponse.Jobs[0].Result.Stderr)
	}
}

func NewSpectrumRestV2(scaleConfig settings.Clusters) (SpectrumScaleConnector, error) {
	glog.V(4).Infof("rest_v2 NewSpectrumRestV2.")

	guiHost := scaleConfig.RestAPI[0].GuiHost
	guiUser := scaleConfig.MgmtUsername
	guiPwd := scaleConfig.MgmtPassword
	guiPort := scaleConfig.RestAPI[0].GuiPort
	if guiPort == 0 {
		guiPort = settings.DefaultGuiPort
	}

	var tr *http.Transport
	endpoint := fmt.Sprintf("%s://%s:%d/", settings.GuiProtocol, guiHost, guiPort)

	if scaleConfig.SecureSslMode {
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(scaleConfig.CacertValue); !ok {
			return &spectrumRestV2{}, fmt.Errorf("Parsing CA cert %v failed", scaleConfig.Cacert)
		}
		tr = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caCertPool, MinVersion: tls.VersionTLS12}}
		glog.V(4).Infof("Created Spectrum Scale connector with SSL mode for %v", guiHost)
	} else {
		//#nosec G402 InsecureSkipVerify was requested by user.
		tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec
		glog.V(4).Infof("Created Spectrum Scale connector without SSL mode for %v", guiHost)
	}

	return &spectrumRestV2{
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   time.Second * 60,
		},
		endpoint: endpoint,
		user:     guiUser,
		password: guiPwd,
	}, nil
}

func (s *spectrumRestV2) GetClusterId() (string, error) {
	glog.V(4).Infof("rest_v2 GetClusterId")

	getClusterURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/cluster")
	getClusterResponse := GetClusterResponse{}

	err := s.doHTTP(getClusterURL, "GET", &getClusterResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get cluster ID: %v", err)
		return "", err
	}
	cid_str := fmt.Sprintf("%v", getClusterResponse.Cluster.ClusterSummary.ClusterID)
	return cid_str, nil
}

// GetClusterSummary function returns the information details of the cluster.
func (s *spectrumRestV2) GetClusterSummary() (ClusterSummary, error) {
	glog.V(4).Infof("rest_v2 GetClusterSummary")

	getClusterURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/cluster")
	getClusterResponse := GetClusterResponse{}

	err := s.doHTTP(getClusterURL, "GET", &getClusterResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get cluster summary: %v", err)
		return ClusterSummary{}, err
	}
	return getClusterResponse.Cluster.ClusterSummary, nil
}

func (s *spectrumRestV2) GetTimeZoneOffset() (string, error) {
	glog.V(4).Infof("rest_v2 GetTimeZoneOffset")

	getConfigURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/config")
	getConfigResponse := GetConfigResponse{}

	err := s.doHTTP(getConfigURL, "GET", &getConfigResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get cluster configuration: %v", err)
		return "", err
	}
	timezone := fmt.Sprintf("%v", getConfigResponse.Config.ClusterConfig.TimeZoneOffset)
	return timezone, nil
}

func (s *spectrumRestV2) GetScaleVersion() (string, error) {
	glog.V(4).Infof("rest_v2 GetScaleVersion")

	getVersionURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/info")
	getVersionResponse := GetInfoResponse_v2{}

	err := s.doHTTP(getVersionURL, "GET", &getVersionResponse, nil)
	if err != nil {
		glog.Errorf("unable to get Spectrum Scale version: [%v]", err)
		return "", err
	}

	if len(getVersionResponse.Info.ServerVersion) == 0 {
		return "", fmt.Errorf("unable to get Spectrum Scale version.")
	}

	return getVersionResponse.Info.ServerVersion, nil
}

func (s *spectrumRestV2) GetFilesystemMountDetails(filesystemName string) (MountInfo, error) {
	glog.V(4).Infof("rest_v2 GetFilesystemMountDetails. filesystemName: %s", filesystemName)

	getFilesystemURL := fmt.Sprintf("%s%s%s", s.endpoint, "scalemgmt/v2/filesystems/", filesystemName)
	getFilesystemResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(getFilesystemURL, "GET", &getFilesystemResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get filesystem details for %s: %v", filesystemName, err)
		return MountInfo{}, err
	}

	if len(getFilesystemResponse.FileSystems) > 0 {
		return getFilesystemResponse.FileSystems[0].Mount, nil
	} else {
		return MountInfo{}, fmt.Errorf("Unable to fetch mount details for %s", filesystemName)
	}
}

func (s *spectrumRestV2) IsFilesystemMountedOnGUINode(filesystemName string) (bool, error) {
	glog.V(4).Infof("rest_v2 IsFilesystemMountedOnGUINode. filesystemName: %s", filesystemName)

	mountURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s", filesystemName))
	mountResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(mountURL, "GET", &mountResponse, nil)
	if err != nil {
		glog.Errorf("Error in getting filesystem mount details for %s: %v", filesystemName, err)
		return false, err
	}

	if len(mountResponse.FileSystems) > 0 {
		glog.V(4).Infof("filesystem [%s] is [%v] on GUI node", filesystemName, mountResponse.FileSystems[0].Mount.Status)
		if mountResponse.FileSystems[0].Mount.Status == "mounted" {
			return true, nil
		} else if mountResponse.FileSystems[0].Mount.Status == "not mounted" {
			return false, nil
		}
		return false, fmt.Errorf("unable to determine mount status of filesystem %s", filesystemName)
	} else {
		return false, fmt.Errorf("unable to fetch mount details for %s", filesystemName)
	}
}

func (s *spectrumRestV2) ListFilesystems() ([]string, error) {
	glog.V(4).Infof("rest_v2 ListFilesystems")

	listFilesystemsURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/filesystems")
	getFilesystemResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(listFilesystemsURL, "GET", &getFilesystemResponse, nil)
	if err != nil {
		glog.Errorf("Error in listing filesystems: %v", err)
		return nil, err
	}
	fsNumber := len(getFilesystemResponse.FileSystems)
	filesystems := make([]string, fsNumber)
	for i := 0; i < fsNumber; i++ {
		filesystems[i] = getFilesystemResponse.FileSystems[i].Name
	}
	return filesystems, nil
}

func (s *spectrumRestV2) GetFilesystemMountpoint(filesystemName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetFilesystemMountpoint. filesystemName: %s", filesystemName)

	getFilesystemURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s", filesystemName))
	getFilesystemResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(getFilesystemURL, "GET", &getFilesystemResponse, nil)
	if err != nil {
		glog.Errorf("Error in getting filesystem details for %s: %v", filesystemName, err)
		return "", err
	}

	if len(getFilesystemResponse.FileSystems) > 0 {
		return getFilesystemResponse.FileSystems[0].Mount.MountPoint, nil
	} else {
		return "", fmt.Errorf("Unable to fetch mount point for %s.", filesystemName)
	}
}

func (s *spectrumRestV2) CopyFsetSnapshotPath(filesystemName string, filesetName string, snapshotName string, srcPath string, targetPath string, nodeclass string) (int, uint64, error) {
	glog.V(4).Infof("rest_v2 CopyFsetSnapshotPath. filesystem: %s, fileset: %s, snapshot: %s, srcPath: %s, targetPath: %s, nodeclass: %s", filesystemName, filesetName, snapshotName, srcPath, targetPath, nodeclass)

	copySnapReq := CopySnapshotRequest{}
	copySnapReq.TargetPath = targetPath

	if nodeclass != "" {
		copySnapReq.NodeClass = nodeclass
	}

	formattedSrcPath := strings.ReplaceAll(srcPath, "/", "%2F")
	copySnapURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshotCopy/%s/path/%s", filesystemName, filesetName, snapshotName, formattedSrcPath))
	copySnapResp := GenericResponse{}

	err := s.doHTTP(copySnapURL, "PUT", &copySnapResp, copySnapReq)
	if err != nil {
		glog.Errorf("Error in copy snapshot request: %v", err)
		return 0, 0, err
	}

	err = s.isRequestAccepted(copySnapResp, copySnapURL)
	if err != nil {
		glog.Errorf("request not accepted for processing: %v", err)
		return 0, 0, err
	}

	return copySnapResp.Status.Code, copySnapResp.Jobs[0].JobID, nil
}

func (s *spectrumRestV2) WaitForJobCompletion(statusCode int, jobID uint64) error {
	glog.V(4).Infof("rest_v2 WaitForJobCompletion. statusCode: %v, jobID: %v", statusCode, jobID)

	err := s.waitForJobCompletion(statusCode, jobID)
	if err != nil {
		glog.Errorf("error in waiting for job completion %v, %v", jobID, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) CopyFilesetPath(filesystemName string, filesetName string, srcPath string, targetPath string, nodeclass string) (int, uint64, error) {
	glog.V(4).Infof("rest_v2 CopyFilesetPath. filesystem: %s, fileset: %s, srcPath: %s, targetPath: %s, nodeclass: %s", filesystemName, filesetName, srcPath, targetPath, nodeclass)

	copyVolReq := CopyVolumeRequest{}
	copyVolReq.TargetPath = targetPath

	if nodeclass != "" {
		copyVolReq.NodeClass = nodeclass
	}

	formattedSrcPath := strings.ReplaceAll(srcPath, "/", "%2F")
	copyVolURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/directoryCopy/%s", filesystemName, filesetName, formattedSrcPath))
	copyVolResp := GenericResponse{}

	err := s.doHTTP(copyVolURL, "PUT", &copyVolResp, copyVolReq)
	if err != nil {
		glog.Errorf("Error in copy volume request: %v", err)
		return 0, 0, err
	}

	err = s.isRequestAccepted(copyVolResp, copyVolURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return 0, 0, err
	}

	return copyVolResp.Status.Code, copyVolResp.Jobs[0].JobID, nil
}

func (s *spectrumRestV2) CopyDirectoryPath(filesystemName string, srcPath string, targetPath string, nodeclass string) (int, uint64, error) {
	glog.V(4).Infof("rest_v2 CopyDirectoryPath. filesystem: %s, srcPath: %s, targetPath: %s, nodeclass: %s", filesystemName, srcPath, targetPath, nodeclass)

	copyVolReq := CopyVolumeRequest{}
	copyVolReq.TargetPath = targetPath

	if nodeclass != "" {
		copyVolReq.NodeClass = nodeclass
	}

	formattedSrcPath := strings.ReplaceAll(srcPath, "/", "%2F")
	copyVolURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directoryCopy/%s", filesystemName, formattedSrcPath))
	copyVolResp := GenericResponse{}

	err := s.doHTTP(copyVolURL, "PUT", &copyVolResp, copyVolReq)
	if err != nil {
		glog.Errorf("Error in copy volume request: %v", err)
		return 0, 0, err
	}

	err = s.isRequestAccepted(copyVolResp, copyVolURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return 0, 0, err
	}

	return copyVolResp.Status.Code, copyVolResp.Jobs[0].JobID, nil
}

func (s *spectrumRestV2) CreateSnapshot(filesystemName string, filesetName string, snapshotName string) error {
	glog.V(4).Infof("rest_v2 CreateSnapshot. filesystem: %s, fileset: %s, snapshot: %v", filesystemName, filesetName, snapshotName)

	snapshotreq := CreateSnapshotRequest{}
	snapshotreq.SnapshotName = snapshotName

	createSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots", filesystemName, filesetName))
	createSnapshotResponse := GenericResponse{}

	err := s.doHTTP(createSnapshotURL, "POST", &createSnapshotResponse, snapshotreq)
	if err != nil {
		glog.Errorf("error in create snapshot request: %v", err)
		return err
	}

	err = s.isRequestAccepted(createSnapshotResponse, createSnapshotURL)
	if err != nil {
		glog.Errorf("request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(createSnapshotResponse.Status.Code, createSnapshotResponse.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSP1102C") { // job failed as snapshot already exists
			fmt.Println(err)
			return nil
		}
		glog.Errorf("unable to create snapshot %s: %v", snapshotName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) DeleteSnapshot(filesystemName string, filesetName string, snapshotName string) error {
	glog.V(4).Infof("rest_v2 DeleteSnapshot. filesystem: %s, fileset: %s, snapshot: %v", filesystemName, filesetName, snapshotName)

	deleteSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots/%s", filesystemName, filesetName, snapshotName))
	deleteSnapshotResponse := GenericResponse{}

	err := s.doHTTP(deleteSnapshotURL, "DELETE", &deleteSnapshotResponse, nil)
	if err != nil {
		glog.Errorf("Error in delete snapshot request: %v", err)
		return err
	}

	err = s.isRequestAccepted(deleteSnapshotResponse, deleteSnapshotURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(deleteSnapshotResponse.Status.Code, deleteSnapshotResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Unable to delete snapshot %s: %v", snapshotName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) GetLatestFilesetSnapshots(filesystemName string, filesetName string) ([]Snapshot_v2, error) {
	glog.V(4).Infof("rest_v2 GetLatestFilesetSnapshots. filesystem: %s, fileset: %s", filesystemName, filesetName)

	getLatestFilesetSnapshotsURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots/latest", filesystemName, filesetName))
	getLatestFilesetSnapshotsResponse := GetSnapshotResponse_v2{}

	err := s.doHTTP(getLatestFilesetSnapshotsURL, "GET", &getLatestFilesetSnapshotsResponse, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to get latest list of snapshots for fileset [%v]. Error [%v]", filesetName, err)
	}

	return getLatestFilesetSnapshotsResponse.Snapshots, nil
}

func (s *spectrumRestV2) UpdateFileset(filesystemName string, filesetName string, opts map[string]interface{}) error {
	glog.V(4).Infof("rest_v2 UpdateFileset. filesystem: %s, fileset: %s, opts: %v", filesystemName, filesetName, opts)
	filesetreq := CreateFilesetRequest{}
	inodeLimit, inodeLimitSpecified := opts[UserSpecifiedInodeLimit]
	if inodeLimitSpecified {
		filesetreq.MaxNumInodes = inodeLimit.(string)
		//filesetreq.AllocInodes = "1024"
	}
	updateFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s", filesystemName, filesetName))
	updateFilesetResponse := GenericResponse{}
	err := s.doHTTP(updateFilesetURL, "PUT", &updateFilesetResponse, filesetreq)
	if err != nil {
		glog.Errorf("error in update fileset request: %v", err)
		return err
	}

	err = s.isRequestAccepted(updateFilesetResponse, updateFilesetURL)
	if err != nil {
		glog.Errorf("request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(updateFilesetResponse.Status.Code, updateFilesetResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("unable to update fileset %s: %v", filesetName, err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) CreateFileset(filesystemName string, filesetName string, opts map[string]interface{}) error {
	glog.V(4).Infof("rest_v2 CreateFileset. filesystem: %s, fileset: %s, opts: %v", filesystemName, filesetName, opts)

	filesetreq := CreateFilesetRequest{}
	filesetreq.FilesetName = filesetName
	filesetreq.Comment = FilesetComment

	filesetType, filesetTypeSpecified := opts[UserSpecifiedFilesetType]
	inodeLimit, inodeLimitSpecified := opts[UserSpecifiedInodeLimit]

	if !filesetTypeSpecified {
		filesetType, filesetTypeSpecified = opts[UserSpecifiedFilesetTypeDep]
	}

	if !inodeLimitSpecified {
		inodeLimit, inodeLimitSpecified = opts[UserSpecifiedInodeLimitDep]
	}

	if filesetTypeSpecified && filesetType.(string) == "dependent" {
		/* Add fileset for dependent fileset-name: */
		parentFileSetName, parentFileSetNameSpecified := opts[UserSpecifiedParentFset]
		if parentFileSetNameSpecified {
			filesetreq.InodeSpace = parentFileSetName.(string)
		} else {
			filesetreq.InodeSpace = "root"
		}
	} else {
		filesetreq.InodeSpace = "new"
		if inodeLimitSpecified {
			filesetreq.MaxNumInodes = inodeLimit.(string)
			filesetreq.AllocInodes = "1024"
		}
	}

	uid, uidSpecified := opts[UserSpecifiedUID]
	gid, gidSpecified := opts[UserSpecifiedGID]
	permissions, permissionsSpecified := opts[UserSpecifiedPermissions]

	if uidSpecified && gidSpecified {
		filesetreq.Owner = fmt.Sprintf("%s:%s", uid, gid)
	} else if uidSpecified {
		filesetreq.Owner = fmt.Sprintf("%s", uid)
	}
	if permissionsSpecified {
		filesetreq.Permissions = fmt.Sprintf("%s", permissions)
	}

	createFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets", filesystemName))
	createFilesetResponse := GenericResponse{}

	err := s.doHTTP(createFilesetURL, "POST", &createFilesetResponse, filesetreq)
	if err != nil {
		glog.Errorf("Error in create fileset request: %v", err)
		return err
	}

	err = s.isRequestAccepted(createFilesetResponse, createFilesetURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(createFilesetResponse.Status.Code, createFilesetResponse.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSP1102C") { // job failed as fileset already exists
			fmt.Println(err)
			return nil
		}
		glog.Errorf("Unable to create fileset %s: %v", filesetName, err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) DeleteFileset(filesystemName string, filesetName string) error {
	glog.V(4).Infof("rest_v2 DeleteFileset. filesystem: %s, fileset: %s", filesystemName, filesetName)

	deleteFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s", filesystemName, filesetName))
	deleteFilesetResponse := GenericResponse{}

	err := s.doHTTP(deleteFilesetURL, "DELETE", &deleteFilesetResponse, nil)
	if err != nil {
		if strings.Contains(deleteFilesetResponse.Status.Message, "Invalid value in 'fsetName'") { // job failed as dir already exists
			glog.Infof("Fileset would have been deleted. So returning success %v", err)
			return nil
		}

		glog.Errorf("Error in delete fileset request: %v", err)
		return err
	}

	err = s.isRequestAccepted(deleteFilesetResponse, deleteFilesetURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(deleteFilesetResponse.Status.Code, deleteFilesetResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Unable to delete fileset %s: %v", filesetName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) LinkFileset(filesystemName string, filesetName string, linkpath string) error {
	glog.V(4).Infof("rest_v2 LinkFileset. filesystem: %s, fileset: %s, linkpath: %s", filesystemName, filesetName, linkpath)

	linkReq := LinkFilesetRequest{}
	linkReq.Path = linkpath
	linkFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/link", filesystemName, filesetName))
	linkFilesetResponse := GenericResponse{}

	err := s.doHTTP(linkFilesetURL, "POST", &linkFilesetResponse, linkReq)
	if err != nil {
		glog.Errorf("Error in link fileset request: %v", err)
		return err
	}

	err = s.isRequestAccepted(linkFilesetResponse, linkFilesetURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(linkFilesetResponse.Status.Code, linkFilesetResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Error in linking fileset %s: %v", filesetName, err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) UnlinkFileset(filesystemName string, filesetName string) error {
	glog.V(4).Infof("rest_v2 UnlinkFileset. filesystem: %s, fileset: %s", filesystemName, filesetName)

	unlinkFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/link?force=True", filesystemName, filesetName))
	unlinkFilesetResponse := GenericResponse{}

	err := s.doHTTP(unlinkFilesetURL, "DELETE", &unlinkFilesetResponse, nil)

	if err != nil {
		glog.Errorf("Error in unlink fileset request: %v", err)
		return err
	}

	err = s.isRequestAccepted(unlinkFilesetResponse, unlinkFilesetURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(unlinkFilesetResponse.Status.Code, unlinkFilesetResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Error in unlink fileset %s: %v", filesetName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) ListFileset(filesystemName string, filesetName string) (Fileset_v2, error) {
	glog.V(4).Infof("rest_v2 ListFileset. filesystem: %s, fileset: %s", filesystemName, filesetName)

	getFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s", filesystemName, filesetName))
	getFilesetResponse := GetFilesetResponse_v2{}

	err := s.doHTTP(getFilesetURL, "GET", &getFilesetResponse, nil)
	if err != nil {
		glog.Errorf("Error in list fileset request: %v", err)
		return Fileset_v2{}, err
	}

	if len(getFilesetResponse.Filesets) == 0 {
		glog.Errorf("No fileset returned for %s", filesetName)
		return Fileset_v2{}, fmt.Errorf("No fileset returned for %s", filesetName)
	}

	return getFilesetResponse.Filesets[0], nil
}

func (s *spectrumRestV2) GetFilesetsInodeSpace(filesystemName string, inodeSpace int) ([]Fileset_v2, error) {
	glog.V(4).Infof("rest_v2 ListAllFilesets. filesystem: %s", filesystemName)

	getFilesetsURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets?filter=config.inodeSpace=%d", filesystemName, inodeSpace))
	getFilesetsResponse := GetFilesetResponse_v2{}

	err := s.doHTTP(getFilesetsURL, "GET", &getFilesetsResponse, nil)
	if err != nil {
		glog.Errorf("Error in list filesets request: %v", err)
		return nil, err
	}

	return getFilesetsResponse.Filesets, nil
}

func (s *spectrumRestV2) IsFilesetLinked(filesystemName string, filesetName string) (bool, error) {
	glog.V(4).Infof("rest_v2 IsFilesetLinked. filesystem: %s, fileset: %s", filesystemName, filesetName)

	fileset, err := s.ListFileset(filesystemName, filesetName)
	if err != nil {
		return false, err
	}

	if (fileset.Config.Path == "") ||
		(fileset.Config.Path == "--") {
		return false, nil
	}
	return true, nil
}

func (s *spectrumRestV2) FilesetRefreshTask() error {
	glog.V(4).Infof("rest_v2 FilesetRefreshTask")

	filesetRefreshURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/refreshTask/enqueue?taskId=FILESETS&maxDelay=0")
	filesetRefreshResponse := GenericResponse{}

	err := s.doHTTP(filesetRefreshURL, "POST", &filesetRefreshResponse, nil)
	if err != nil {
		glog.Errorf("Error in fileset refresh task: %v", err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) MakeDirectory(filesystemName string, relativePath string, uid string, gid string) error {
	glog.V(4).Infof("rest_v2 MakeDirectory. filesystem: %s, path: %s, uid: %s, gid: %s", filesystemName, relativePath, uid, gid)

	dirreq := CreateMakeDirRequest{}

	if uid != "" {
		_, err := strconv.Atoi(uid)
		if err != nil {
			dirreq.USER = uid
		} else {
			dirreq.UID = uid
		}
	} else {
		dirreq.UID = "0"
	}

	if gid != "" {
		_, err := strconv.Atoi(gid)
		if err != nil {
			dirreq.GROUP = gid
		} else {
			dirreq.GID = gid
		}
	} else {
		dirreq.GID = "0"
	}

	formattedPath := strings.ReplaceAll(relativePath, "/", "%2F")
	makeDirURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directory/%s", filesystemName, formattedPath))

	makeDirResponse := GenericResponse{}

	err := s.doHTTP(makeDirURL, "POST", &makeDirResponse, dirreq)

	if err != nil {
		glog.Errorf("Error in make directory request: %v", err)
		return err
	}

	err = s.isRequestAccepted(makeDirResponse, makeDirURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(makeDirResponse.Status.Code, makeDirResponse.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSG0762C") { // job failed as dir already exists
			glog.Infof("Directory exists. %v", err)
			return nil
		}

		glog.Errorf("Unable to make directory %s: %v.", relativePath, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) MakeDirectoryV2(filesystemName string, relativePath string, uid string, gid string, permissions string) error {
	glog.V(4).Infof("rest_v2 MakeDirectoryV2. filesystem: %s, path: %s, uid: %s, gid: %s, permissions: %s", filesystemName, relativePath, uid, gid, permissions)

	dirreq := CreateMakeDirRequest{}

	if uid != "" {
		_, err := strconv.Atoi(uid)
		if err != nil {
			dirreq.USER = uid
		} else {
			dirreq.UID = uid
		}
	} else {
		dirreq.UID = "0"
	}

	if gid != "" {
		_, err := strconv.Atoi(gid)
		if err != nil {
			dirreq.GROUP = gid
		} else {
			dirreq.GID = gid
		}
	} else {
		dirreq.GID = "0"
	}

	dirreq.PERMISSIONS = permissions

	formattedPath := strings.ReplaceAll(relativePath, "/", "%2F")
	makeDirURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directory/%s", filesystemName, formattedPath))

	makeDirResponse := GenericResponse{}

	err := s.doHTTP(makeDirURL, "POST", &makeDirResponse, dirreq)

	if err != nil {
		glog.Errorf("Error in make directory request: %v", err)
		return err
	}

	err = s.isRequestAccepted(makeDirResponse, makeDirURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(makeDirResponse.Status.Code, makeDirResponse.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSG0762C") { // job failed as dir already exists
			glog.Infof("Directory exists. %v", err)
			return nil
		}

		glog.Errorf("Unable to make directory %s: %v.", relativePath, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) SetFilesetQuota(filesystemName string, filesetName string, quota string) error {
	glog.V(4).Infof("rest_v2 SetFilesetQuota. filesystem: %s, fileset: %s, quota: %s", filesystemName, filesetName, quota)

	setQuotaURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/quotas", filesystemName))
	quotaRequest := SetQuotaRequest_v2{}

	quotaRequest.BlockHardLimit = quota
	quotaRequest.BlockSoftLimit = quota
	quotaRequest.OperationType = "setQuota"
	quotaRequest.QuotaType = "fileset"
	quotaRequest.ObjectName = filesetName

	setQuotaResponse := GenericResponse{}

	err := s.doHTTP(setQuotaURL, "POST", &setQuotaResponse, quotaRequest)
	if err != nil {
		glog.Errorf("Error in set fileset quota request: %v", err)
		return err
	}

	err = s.isRequestAccepted(setQuotaResponse, setQuotaURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(setQuotaResponse.Status.Code, setQuotaResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Unable to set quota for fileset %s: %v", filesetName, err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) CheckIfFSQuotaEnabled(filesystemName string) error {
	glog.V(4).Infof("rest_v2 CheckIfFSQuotaEnabled. filesystem: %s", filesystemName)

	checkQuotaURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/quotas", filesystemName))
	QuotaResponse := GetQuotaResponse_v2{}

	err := s.doHTTP(checkQuotaURL, "GET", &QuotaResponse, nil)
	if err != nil {
		glog.Errorf("Error in check quota: %v", err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) IsValidNodeclass(nodeclass string) (bool, error) {
	glog.V(4).Infof("rest_v2 IsValidNodeclass. nodeclass: %s", nodeclass)

	checkNodeclassURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/nodeclasses/%s", nodeclass))
	nodeclassResponse := GenericResponse{}

	err := s.doHTTP(checkNodeclassURL, "GET", &nodeclassResponse, nil)
	if err != nil {
		if strings.Contains(nodeclassResponse.Status.Message, "Invalid value in nodeclassName") {
			// nodeclass is not present
			return false, nil
		}
		return false, fmt.Errorf("unable to get nodeclass details")
	}
	return true, nil
}

func (s *spectrumRestV2) IsSnapshotSupported() (bool, error) {
	glog.V(4).Infof("rest_v2 IsSnapshotSupported")

	getVersionURL := utils.FormatURL(s.endpoint, "scalemgmt/v2/info")
	getVersionResponse := GetInfoResponse_v2{}

	err := s.doHTTP(getVersionURL, "GET", &getVersionResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get cluster information: [%v]", err)
		return false, err
	}

	if len(getVersionResponse.Info.Paths.SnapCopyOp) == 0 {
		return false, nil
	}

	return true, nil
}

func (s *spectrumRestV2) GetFilesetQuotaDetails(filesystemName string, filesetName string) (Quota_v2, error) {
	glog.V(4).Infof("rest_v2 GetFilesetQuotaDetails. filesystem: %s, fileset: %s", filesystemName, filesetName)

	listQuotaURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/quotas?filter=objectName=%s", filesystemName, filesetName))
	listQuotaResponse := GetQuotaResponse_v2{}

	err := s.doHTTP(listQuotaURL, "GET", &listQuotaResponse, nil)
	if err != nil {
		glog.Errorf("Unable to fetch quota information for fileset %s:%s: [%v]", filesystemName, filesetName, err)
		return Quota_v2{}, err
	}

	if len(listQuotaResponse.Quotas) == 0 {
		glog.Errorf("No quota information found for fileset %s:%s ", filesystemName, filesetName)
		return Quota_v2{}, nil
	}

	return listQuotaResponse.Quotas[0], nil
}

func (s *spectrumRestV2) ListFilesetQuota(filesystemName string, filesetName string) (string, error) {
	glog.V(4).Infof("rest_v2 ListFilesetQuota. filesystem: %s, fileset: %s", filesystemName, filesetName)

	listQuotaResponse, err := s.GetFilesetQuotaDetails(filesystemName, filesetName)

	if err != nil {
		return "", err
	}

	if listQuotaResponse.BlockLimit > 0 {
		return fmt.Sprintf("%dK", listQuotaResponse.BlockLimit), nil
	} else {
		glog.Errorf("No quota information found for fileset %s", filesetName)
		return "", nil
	}
}

func (s *spectrumRestV2) doHTTP(endpoint string, method string, responseObject interface{}, param interface{}) error {
	glog.V(4).Infof("rest_v2 doHTTP. endpoint: %s, method: %s, param: %v", endpoint, method, param)

	response, err := utils.HttpExecuteUserAuth(s.httpClient, method, endpoint, s.user, s.password, param)
	if err != nil {
		glog.Errorf("Error in authentication request: %v", err)
		return err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		return status.Error(codes.Unauthenticated, fmt.Sprintf("Unauthorized %s request to %v: %v", method, endpoint, response.Status))
	}

	err = utils.UnmarshalResponse(response, responseObject)
	if err != nil {
		return err
	}

	if !s.isStatusOK(response.StatusCode) {
		return fmt.Errorf("Remote call completed with error [%v]. Error in response [%v]", response.Status, responseObject)
	}

	return nil
}

func (s *spectrumRestV2) MountFilesystem(filesystemName string, nodeName string) error { //nolint:dupl
	glog.V(4).Infof("rest_v2 MountFilesystem. filesystem: %s, node: %s", filesystemName, nodeName)

	mountreq := MountFilesystemRequest{}
	mountreq.Nodes = append(mountreq.Nodes, nodeName)

	mountFilesystemURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/mount", filesystemName))
	mountFilesystemResponse := GenericResponse{}

	err := s.doHTTP(mountFilesystemURL, "PUT", &mountFilesystemResponse, mountreq)
	if err != nil {
		glog.Errorf("Error in mount filesystem request: %v", err)
		return err
	}

	err = s.isRequestAccepted(mountFilesystemResponse, mountFilesystemURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(mountFilesystemResponse.Status.Code, mountFilesystemResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Unable to Mount filesystem %s on node %s: %v", filesystemName, nodeName, err)
		return err
	}
	return nil
}

func (s *spectrumRestV2) UnmountFilesystem(filesystemName string, nodeName string) error { //nolint:dupl
	glog.V(4).Infof("rest_v2 UnmountFilesystem. filesystem: %s, node: %s", filesystemName, nodeName)

	unmountreq := UnmountFilesystemRequest{}
	unmountreq.Nodes = append(unmountreq.Nodes, nodeName)

	unmountFilesystemURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/unmount", filesystemName))
	unmountFilesystemResponse := GenericResponse{}

	err := s.doHTTP(unmountFilesystemURL, "PUT", &unmountFilesystemResponse, unmountreq)
	if err != nil {
		glog.Errorf("Error in unmount filesystem request: %v", err)
		return err
	}

	err = s.isRequestAccepted(unmountFilesystemResponse, unmountFilesystemURL)
	if err != nil {
		glog.Errorf("Request not accepted for processing: %v", err)
		return err
	}

	err = s.waitForJobCompletion(unmountFilesystemResponse.Status.Code, unmountFilesystemResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("Unable to unmount filesystem %s on node %s: %v", filesystemName, nodeName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) GetFilesystemName(filesystemUUID string) (string, error) { //nolint:dupl
	glog.V(4).Infof("rest_v2 GetFilesystemName. UUID: %s", filesystemUUID)

	getFilesystemNameURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems?filter=uuid=%s", filesystemUUID))
	getFilesystemNameURLResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(getFilesystemNameURL, "GET", &getFilesystemNameURLResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get filesystem name for uuid %s: %v", filesystemUUID, err)
		return "", err
	}

	if len(getFilesystemNameURLResponse.FileSystems) == 0 {
		glog.Errorf("Unable to fetch filesystem name details for %s", filesystemUUID)
		return "", fmt.Errorf("Unable to fetch filesystem name details for %s", filesystemUUID)
	}
	return getFilesystemNameURLResponse.FileSystems[0].Name, nil
}

func (s *spectrumRestV2) GetFilesystemDetails(filesystemName string) (FileSystem_v2, error) { //nolint:dupl
	glog.V(4).Infof("rest_v2 GetFilesystemDetails. Name: %s", filesystemName)

	getFilesystemDetailsURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s", filesystemName))
	getFilesystemDetailsURLResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(getFilesystemDetailsURL, "GET", &getFilesystemDetailsURLResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get filesystem details for filesystem %s: %v", filesystemName, err)
		return FileSystem_v2{}, err
	}

	if len(getFilesystemDetailsURLResponse.FileSystems) == 0 {
		glog.Errorf("Unable to fetch filesystem details for %s", filesystemName)
		return FileSystem_v2{}, fmt.Errorf("Unable to fetch filesystem details for %s", filesystemName)
	}

	return getFilesystemDetailsURLResponse.FileSystems[0], nil
}

func (s *spectrumRestV2) GetFsUid(filesystemName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetFsUid. filesystem: %s", filesystemName)

	getFilesystemURL := fmt.Sprintf("%s%s%s", s.endpoint, "scalemgmt/v2/filesystems/", filesystemName)
	getFilesystemResponse := GetFilesystemResponse_v2{}

	err := s.doHTTP(getFilesystemURL, "GET", &getFilesystemResponse, nil)
	if err != nil {
		return "", fmt.Errorf("Unable to get filesystem details for %s", filesystemName)
	}

	fmt.Println(getFilesystemResponse)
	if len(getFilesystemResponse.FileSystems) > 0 {
		return getFilesystemResponse.FileSystems[0].UUID, nil
	} else {
		return "", fmt.Errorf("Unable to fetch mount details for %s", filesystemName)
	}
}

func (s *spectrumRestV2) DeleteSymLnk(filesystemName string, LnkName string) error {
	glog.V(4).Infof("rest_v2 DeleteSymLnk. filesystem: %s, link: %s", filesystemName, LnkName)

	LnkName = strings.ReplaceAll(LnkName, "/", "%2F")
	deleteLnkURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/symlink/%s", filesystemName, LnkName))
	deleteLnkResponse := GenericResponse{}

	err := s.doHTTP(deleteLnkURL, "DELETE", &deleteLnkResponse, nil)
	if err != nil {
		return fmt.Errorf("Unable to delete Symlink %v.", LnkName)
	}

	err = s.isRequestAccepted(deleteLnkResponse, deleteLnkURL)
	if err != nil {
		return err
	}

	err = s.waitForJobCompletion(deleteLnkResponse.Status.Code, deleteLnkResponse.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSG2006C") {
			glog.V(4).Infof("Since slink %v was already deleted, so returning success", LnkName)
			return nil
		}
		return fmt.Errorf("Unable to delete symLnk %v:%v.", LnkName, err)
	}

	return nil
}

func (s *spectrumRestV2) DeleteDirectory(filesystemName string, dirName string, safe bool) error {
	glog.V(4).Infof("rest_v2 DeleteDirectory. filesystem: %s, dir: %s, safe: %v", filesystemName, dirName, safe)

	NdirName := strings.ReplaceAll(dirName, "/", "%2F")
	deleteDirURL := ""
	if safe {
		deleteDirURL = utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directory/%s?safe=True", filesystemName, NdirName))
	} else {
		deleteDirURL = utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directory/%s", filesystemName, NdirName))
	}
	deleteDirResponse := GenericResponse{}

	err := s.doHTTP(deleteDirURL, "DELETE", &deleteDirResponse, nil)
	if err != nil {
		return fmt.Errorf("Unable to delete dir %v.", dirName)
	}

	err = s.isRequestAccepted(deleteDirResponse, deleteDirURL)
	if err != nil {
		return err
	}

	err = s.waitForJobCompletion(deleteDirResponse.Status.Code, deleteDirResponse.Jobs[0].JobID)
	if err != nil {
		return fmt.Errorf("Unable to delete dir %v:%v", dirName, err)
	}

	return nil
}

func (s *spectrumRestV2) StatDirectory(filesystemName string, dirName string) (string, error) {
	glog.V(4).Infof("rest_v2 StatDirectory. filesystem: %s, dir: %s", filesystemName, dirName)

	fmtDirName := strings.ReplaceAll(dirName, "/", "%2F")
	statDirURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/directory/%s", filesystemName, fmtDirName))
	statDirResponse := GenericResponse{}

	err := s.doHTTP(statDirURL, "GET", &statDirResponse, nil)
	if err != nil {
		return "", fmt.Errorf("Unable to stat dir %v.", dirName)
	}

	err = s.isRequestAccepted(statDirResponse, statDirURL)
	if err != nil {
		return "", err
	}

	jobResp, err := s.waitForJobCompletionWithResp(statDirResponse.Status.Code, statDirResponse.Jobs[0].JobID)
	if err != nil {
		return "", fmt.Errorf("Unable to stat dir %v:%v", dirName, err)
	}

	statInfo := jobResp.Jobs[0].Result.Stdout[0]

	return statInfo, nil
}

func (s *spectrumRestV2) GetFileSetUid(filesystemName string, filesetName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetFileSetUid. filesystem: %s, fileset: %s", filesystemName, filesetName)

	filesetResponse, err := s.GetFileSetResponseFromName(filesystemName, filesetName)
	if err != nil {
		return "", fmt.Errorf("Fileset response not found for fileset %v:%v", filesystemName, filesetName)
	}

	return fmt.Sprintf("%d", filesetResponse.Config.Id), nil
}

func (s *spectrumRestV2) GetFileSetResponseFromName(filesystemName string, filesetName string) (Fileset_v2, error) {
	glog.V(4).Infof("rest_v2 GetFileSetResponseFromName. filesystem: %s, fileset: %s", filesystemName, filesetName)

	getFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s", filesystemName, filesetName))
	getFilesetResponse := GetFilesetResponse_v2{}

	err := s.doHTTP(getFilesetURL, "GET", &getFilesetResponse, nil)
	if err != nil {
		return Fileset_v2{}, fmt.Errorf("Unable to list fileset %v.", filesetName)
	}

	if len(getFilesetResponse.Filesets) == 0 {
		return Fileset_v2{}, fmt.Errorf("Unable to list fileset %v.", filesetName)
	}

	return getFilesetResponse.Filesets[0], nil
}

// CheckIfFilesetExist Checking if fileset exist in filesystem
func (s *spectrumRestV2) CheckIfFilesetExist(filesystemName string, filesetName string) (bool, error) {
	glog.V(4).Infof("rest_v2 CheckIfFilesetExist. filesystem: %s, fileset: %s", filesystemName, filesetName)

	checkFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s", filesystemName, filesetName))
	getFilesetResponse := GetFilesetResponse_v2{}

	err := s.doHTTP(checkFilesetURL, "GET", &getFilesetResponse, nil)
	if err != nil {
		if strings.Contains(getFilesetResponse.Status.Message, "Invalid value in 'filesetName'") {
			// snapshot is not present
			return false, nil
		}
		return false, fmt.Errorf("unable to get fileset details for filesystem: %v, fileset: %v", filesystemName, filesetName)
	}
	return true, nil
}

func (s *spectrumRestV2) GetFileSetNameFromId(filesystemName string, Id string) (string, error) {
	glog.V(4).Infof("rest_v2 GetFileSetNameFromId. filesystem: %s, fileset id: %s", filesystemName, Id)

	filesetResponse, err := s.GetFileSetResponseFromId(filesystemName, Id)
	if err != nil {
		return "", fmt.Errorf("Fileset response not found for fileset Id %v:%v", filesystemName, Id)
	}
	return filesetResponse.FilesetName, nil
}

func (s *spectrumRestV2) GetFileSetResponseFromId(filesystemName string, Id string) (Fileset_v2, error) {
	glog.V(4).Infof("rest_v2 GetFileSetResponseFromId. filesystem: %s, fileset id: %s", filesystemName, Id)

	getFilesetURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets?filter=config.id=%s", filesystemName, Id))
	getFilesetResponse := GetFilesetResponse_v2{}

	err := s.doHTTP(getFilesetURL, "GET", &getFilesetResponse, nil)
	if err != nil {
		return Fileset_v2{}, fmt.Errorf("unable to get name for fileset Id %v:%v", filesystemName, Id)
	}

	if len(getFilesetResponse.Filesets) == 0 {
		return Fileset_v2{}, fmt.Errorf("no filesets found for Id %v:%v", filesystemName, Id)
	}

	return getFilesetResponse.Filesets[0], nil
}

//nolint:dupl
func (s *spectrumRestV2) GetSnapshotCreateTimestamp(filesystemName string, filesetName string, snapName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetSnapshotCreateTimestamp. filesystem: %s, fileset: %s, snapshot: %s ", filesystemName, filesetName, snapName)

	getSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots/%s", filesystemName, filesetName, snapName))
	getSnapshotResponse := GetSnapshotResponse_v2{}

	err := s.doHTTP(getSnapshotURL, "GET", &getSnapshotResponse, nil)
	if err != nil {
		return "", fmt.Errorf("unable to list snapshot %v", snapName)
	}

	if len(getSnapshotResponse.Snapshots) == 0 {
		return "", fmt.Errorf("unable to list snapshot %v", snapName)
	}

	return fmt.Sprintf(getSnapshotResponse.Snapshots[0].Created), nil
}

//nolint:dupl
func (s *spectrumRestV2) GetSnapshotUid(filesystemName string, filesetName string, snapName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetSnapshotUid. filesystem: %s, fileset: %s, snapshot: %s ", filesystemName, filesetName, snapName)

	getSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots/%s", filesystemName, filesetName, snapName))
	getSnapshotResponse := GetSnapshotResponse_v2{}

	err := s.doHTTP(getSnapshotURL, "GET", &getSnapshotResponse, nil)
	if err != nil {
		return "", fmt.Errorf("unable to list snapshot %v", snapName)
	}

	if len(getSnapshotResponse.Snapshots) == 0 {
		return "", fmt.Errorf("unable to list snapshot %v", snapName)
	}

	return fmt.Sprintf("%d", getSnapshotResponse.Snapshots[0].SnapID), nil
}

// CheckIfSnapshotExist Checking if snapshot exist in fileset
func (s *spectrumRestV2) CheckIfSnapshotExist(filesystemName string, filesetName string, snapshotName string) (bool, error) {
	glog.V(4).Infof("rest_v2 CheckIfSnapshotExist. filesystem: %s, fileset: %s, snapshot: %s ", filesystemName, filesetName, snapshotName)

	getSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots/%s", filesystemName, filesetName, snapshotName))
	getSnapshotResponse := GetSnapshotResponse_v2{}

	err := s.doHTTP(getSnapshotURL, "GET", &getSnapshotResponse, nil)
	if err != nil {
		if strings.Contains(getSnapshotResponse.Status.Message, "Invalid value in 'snapshotName'") && len(getSnapshotResponse.Snapshots) == 0 {
			// snapshot is not present
			return false, nil
		}
		return false, fmt.Errorf("unable to get snapshot details for filesystem: %v, fileset: %v and snapshot: %v", filesystemName, filesetName, snapshotName)
	}
	return true, nil
}

//ListFilesetSnapshots Return list of snapshot under fileset, true if snapshots present
func (s *spectrumRestV2) ListFilesetSnapshots(filesystemName string, filesetName string) ([]Snapshot_v2, error) {
	glog.V(4).Infof("rest_v2 ListFilesetSnapshots. filesystem: %s, fileset: %s", filesystemName, filesetName)

	listFilesetSnapshotURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/filesets/%s/snapshots", filesystemName, filesetName))
	listFilesetSnapshotResponse := GetSnapshotResponse_v2{}

	err := s.doHTTP(listFilesetSnapshotURL, "GET", &listFilesetSnapshotResponse, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to list snapshots for fileset %v. Error [%v]", filesetName, err)
	}

	return listFilesetSnapshotResponse.Snapshots, nil
}

func (s *spectrumRestV2) CheckIfFileDirPresent(filesystemName string, relPath string) (bool, error) {
	glog.V(4).Infof("rest_v2 CheckIfFileDirPresent. filesystem: %s, path: %s", filesystemName, relPath)

	RelPath := strings.ReplaceAll(relPath, "/", "%2F")
	checkFilDirUrl := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/owner/%s", filesystemName, RelPath))
	ownerResp := OwnerResp_v2{}

	err := s.doHTTP(checkFilDirUrl, "GET", &ownerResp, nil)
	if err != nil {
		if strings.Contains(ownerResp.Status.Message, "File not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *spectrumRestV2) CreateSymLink(SlnkfilesystemName string, TargetFs string, relativePath string, LnkPath string) error {
	glog.V(4).Infof("rest_v2 CreateSymLink. SlnkfilesystemName: %s, TargetFs: %s, relativePath: %s, LnkPath: %s", SlnkfilesystemName, TargetFs, relativePath, LnkPath)

	symLnkReq := SymLnkRequest{}
	symLnkReq.FilesystemName = TargetFs
	symLnkReq.RelativePath = relativePath

	LnkPath = strings.ReplaceAll(LnkPath, "/", "%2F")

	symLnkUrl := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/symlink/%s", SlnkfilesystemName, LnkPath))

	makeSlnkResp := GenericResponse{}

	err := s.doHTTP(symLnkUrl, "POST", &makeSlnkResp, symLnkReq)

	if err != nil {
		return err
	}

	err = s.isRequestAccepted(makeSlnkResp, symLnkUrl)
	if err != nil {
		return err
	}

	err = s.waitForJobCompletion(makeSlnkResp.Status.Code, makeSlnkResp.Jobs[0].JobID)
	if err != nil {
		if strings.Contains(err.Error(), "EFSSG0762C") { // job failed as dir already exists
			return nil
		}
	}
	return err
}

func (s *spectrumRestV2) IsNodeComponentHealthy(nodeName string, component string) (bool, error) {
	glog.V(4).Infof("rest_v2 GetNodeHealthStates, nodeName: %s, component: %s", nodeName, component)

	getNodeHealthStatesURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/nodes/%s/health/states?filter=state=HEALTHY,entityType=NODE,component=%s", nodeName, component))
	getNodeHealthStatesResponse := GetNodeHealthStatesResponse_v2{}

	err := s.doHTTP(getNodeHealthStatesURL, "GET", &getNodeHealthStatesResponse, nil)
	if err != nil {
		return false, fmt.Errorf("unable to get health states for nodename %v", nodeName)
	}

	if len(getNodeHealthStatesResponse.States) == 0 {
		return false, nil
	}

	return true, nil
}

func (s *spectrumRestV2) SetFilesystemPolicy(policy *Policy, filesystemName string) error {
	glog.V(4).Infof("rest_v2 setFilesystemPolicy for filesystem %s", filesystemName)

	setPolicyURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/policies", filesystemName))
	setPolicyResponse := GenericResponse{}

	err := s.doHTTP(setPolicyURL, "PUT", &setPolicyResponse, policy)
	if err != nil {
		glog.Errorf("unable to send filesystem policy: %v", setPolicyResponse.Status.Message)
		return err
	}

	err = s.waitForJobCompletion(setPolicyResponse.Status.Code, setPolicyResponse.Jobs[0].JobID)
	if err != nil {
		glog.Errorf("setting policy rule %s for filesystem %s failed with error %v", policy.Policy, filesystemName, err)
		return err
	}

	return nil
}

func (s *spectrumRestV2) DoesTierExist(tierName string, filesystemName string) error {
	glog.V(4).Infof("rest_v2 DoesTierExist. name %s, filesystem %s", tierName, filesystemName)

	_, err := s.GetTierInfoFromName(tierName, filesystemName)
	if err != nil {
		if strings.Contains(err.Error(), "Invalid value in 'storagePool'") {
			return fmt.Errorf("invalid tier '%s' specified for filesystem %s", tierName, filesystemName)
		}
		return err
	}

	return nil
}

func (s *spectrumRestV2) GetTierInfoFromName(tierName string, filesystemName string) (*StorageTier, error) {
	glog.V(4).Infof("rest_v2 GetTierInfoFromName. name %s, filesystem %s", tierName, filesystemName)

	tierUrl := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/pools/%s", filesystemName, tierName))
	getTierResponse := &StorageTiers{}

	err := s.doHTTP(tierUrl, "GET", getTierResponse, nil)
	if err != nil {
		glog.Errorf("Unable to get tier: %s err: %v", tierName, err)
		return nil, err
	}

	if len(getTierResponse.StorageTiers) > 0 {
		return &getTierResponse.StorageTiers[0], nil
	} else {
		return nil, fmt.Errorf("unable to fetch storage tiers for %s", filesystemName)
	}
}

func (s *spectrumRestV2) CheckIfDefaultPolicyPartitionExists(partitionName string, filesystemName string) bool {
	glog.V(4).Infof("rest_v2 CheckIfDefaultPolicyPartitionExists. name %s, filesystem %s", partitionName, filesystemName)

	partitionURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/partition/%s", filesystemName, partitionName))
	getPartitionResponse := GenericResponse{}

	// If it does or doesn't exist and we get an error we will default to just setting it again as an override
	err := s.doHTTP(partitionURL, "GET", &getPartitionResponse, nil)
	return err == nil
}

func (s *spectrumRestV2) GetFirstDataTier(filesystemName string) (string, error) {
	glog.V(4).Infof("rest_v2 GetFirstDataTier. filesystem %s", filesystemName)

	tiersURL := utils.FormatURL(s.endpoint, fmt.Sprintf("scalemgmt/v2/filesystems/%s/pools", filesystemName))
	getTierResponse := &StorageTiers{}

	err := s.doHTTP(tiersURL, "GET", getTierResponse, nil)
	if err != nil {
		return "", err
	}

	for _, tier := range getTierResponse.StorageTiers {
		if tier.StorageTierName == "system" {
			continue
		}

		tierInfo, err := s.GetTierInfoFromName(tier.StorageTierName, tier.FilesystemName)
		if err != nil {
			return "", err
		}
		if tierInfo.TotalDataInKB > 0 {
			glog.V(2).Infof("GetFirstDataTier: Setting default tier to %s", tierInfo.StorageTierName)
			return tierInfo.StorageTierName, nil
		}
	}

	glog.V(2).Infof("GetFirstDataTier: Defaulting to system tier")
	return "system", nil
}
