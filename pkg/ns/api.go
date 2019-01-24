package ns

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// LogIn - log in to NexentaStor API and get auth token
func (nsp *Provider) LogIn() error {
	l := nsp.Log.WithField("func", "LogIn()")

	data := nefAuthLoginRequest{
		Username: nsp.Username,
		Password: nsp.Password,
	}

	_, bodyBytes, err := nsp.RestClient.Send(http.MethodPost, "auth/login", data)
	if err != nil {
		// try to parse error from rest response
		nefError := nsp.parseNefError(bodyBytes, "Login request")
		if nefError != nil {
			if IsAuthNefError(nefError) {
				l.Errorf(
					"login to NexentaStor %s failed (username: '%s'), "+
						"please make sure to use correct address and password",
					nsp.Address,
					nsp.Username)
			}
			return nefError
		}

		return fmt.Errorf("Login request: failed, response: %s; error: %s", bodyBytes, err)
	}

	response := nefAuthLoginResponse{}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return fmt.Errorf("Login request: cannot unmarshal JSON from: '%s' to '%+v': %s", bodyBytes, response, err)
	} else if len(response.Token) == 0 {
		return fmt.Errorf("Login request: token not found in response: '%s'", bodyBytes)
	}

	nsp.RestClient.SetAuthToken(response.Token)
	l.Debugf("login token has been updated")
	return nil
}

// GetLicense - return NexentaStor license
func (nsp *Provider) GetLicense() (license License, err error) {
	err = nsp.sendRequestWithStruct(http.MethodGet, "/settings/license", nil, &license)
	return license, err
}

// GetPools - get NexentaStor pools
func (nsp *Provider) GetPools() ([]Pool, error) {
	uri := nsp.RestClient.BuildURI("/storage/pools", map[string]string{
		"fields": "poolName,health,status",
	})

	response := nefStoragePoolsResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return nil, err
	}

	return response.Data, nil
}

// GetFilesystemAvailableCapacity - get NexentaStor filesystem available size by its path
func (nsp *Provider) GetFilesystemAvailableCapacity(path string) (int64, error) {
	uri := nsp.RestClient.BuildURI("/storage/filesystems", map[string]string{
		"path":   path,
		"fields": "bytesAvailable",
	})

	response := nefStorageFilesystemsResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return 0, err
	}

	var availableSize int64
	if len(response.Data) > 0 {
		availableSize = int64(response.Data[0].BytesAvailable)
	}

	return availableSize, nil
}

// GetFilesystem - get NexentaStor filesystem by its path
func (nsp *Provider) GetFilesystem(path string) (filesystem Filesystem, err error) {
	if len(path) == 0 {
		return filesystem, fmt.Errorf("Filesystem path is empty")
	}

	uri := nsp.RestClient.BuildURI("/storage/filesystems", map[string]string{
		"path":   path,
		"fields": "path,mountPoint,bytesAvailable,bytesUsed,sharedOverNfs,sharedOverSmb",
	})

	response := nefStorageFilesystemsResponse{}
	err = nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return filesystem, err
	}

	if len(response.Data) == 0 {
		return filesystem, &NefError{Code: "ENOENT", Err: fmt.Errorf("Filesystem '%s' not found", path)}
	}

	return response.Data[0], nil
}

// GetFilesystems - get all NexentaStor filesystems by parent filesystem
func (nsp *Provider) GetFilesystems(parent string) ([]Filesystem, error) {
	uri := nsp.RestClient.BuildURI("/storage/filesystems", map[string]string{
		"parent": parent,
		"fields": "path,mountPoint,bytesAvailable,bytesUsed,sharedOverNfs,sharedOverSmb",
	})

	response := nefStorageFilesystemsResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return nil, err
	}

	filesystems := []Filesystem{}
	for _, fs := range response.Data {
		if fs.Path != parent { // exclude parent filesystem from the list
			filesystems = append(filesystems, fs)
		}
	}

	return filesystems, nil
}

// CreateFilesystemParams - params to create filesystem
type CreateFilesystemParams struct {
	// filesystem path w/o leading slash
	Path string `json:"path"`
	// filesystem referenced quota size in bytes
	ReferencedQuotaSize int64 `json:"referencedQuotaSize,omitempty"`
}

// CreateFilesystem - create filesystem by path
func (nsp *Provider) CreateFilesystem(params CreateFilesystemParams) error {
	if params.Path == "" {
		return fmt.Errorf("Parameter 'CreateFilesystemParams.Path' is required")
	}

	return nsp.sendRequest(http.MethodPost, "/storage/filesystems", params)
}

// DestroyFilesystem - destroy filesystem by path
func (nsp *Provider) DestroyFilesystem(path string) error {
	if path == "" {
		return fmt.Errorf("Filesystem path is required")
	}

	uri := nsp.RestClient.BuildURI(
		fmt.Sprintf("/storage/filesystems/%s", url.PathEscape(path)),
		map[string]string{
			"force":     "true",
			"snapshots": "true", //TODO figured out if we can keep snapshots
		},
	)

	return nsp.sendRequest(http.MethodDelete, uri, nil)
}

// PromoteFilesystem promotes a cloned filesystem to be no longer dependent on its original snapshot
func (nsp *Provider) PromoteFilesystem(path string) error {
	if path == "" {
		return fmt.Errorf("Filesystem path is required")
	}

	uri := fmt.Sprintf("/storage/filesystems/%s/promote", url.PathEscape(path))

	return nsp.sendRequest(http.MethodPost, uri, nil)
}

// CreateNfsShareParams - params to create NFS share
type CreateNfsShareParams struct {
	// filesystem path w/o leading slash
	Filesystem string `json:"filesystem"`
}

// CreateNfsShare - create NFS share on specified filesystem
// CLI test:
//	 showmount -e HOST
// 	 mkdir -p /mnt/test && sudo mount -v -t nfs HOST:/pool/fs /mnt/test
// 	 findmnt /mnt/test
func (nsp *Provider) CreateNfsShare(params CreateNfsShareParams) error {
	if params.Filesystem == "" {
		return fmt.Errorf("CreateNfsShareParams.Filesystem is required")
	}

	data := nefNasNfsRequest{
		Filesystem: params.Filesystem,
		Anon:       "root",
		SecurityContexts: []nefNasNfsRequestSecurityContext{
			nefNasNfsRequestSecurityContext{
				SecurityModes: []string{"sys"},
			},
		},
	}

	return nsp.sendRequest(http.MethodPost, "nas/nfs", data)
}

// DeleteNfsShare - destroy NFS chare by filesystem path
func (nsp *Provider) DeleteNfsShare(path string) error {
	if len(path) == 0 {
		return fmt.Errorf("Filesystem path is empty")
	}

	uri := fmt.Sprintf("/nas/nfs/%s", url.PathEscape(path))

	return nsp.sendRequest(http.MethodDelete, uri, nil)
}

// CreateSmbShareParams - params to create SMB share
type CreateSmbShareParams struct {
	// filesystem path w/o leading slash
	Filesystem string `json:"filesystem"`
	// share name, used in mount command
	ShareName string `json:"shareName,omitempty"`
}

// CreateSmbShare - create SMB share (cifs) on specified filesystem
// Leave shareName empty to generate default value
// CLI test:
// 	 mkdir -p /mnt/test && sudo mount -v -t cifs -o username=admin,password=Nexenta@1 //HOST//pool_fs /mnt/test
// 	 findmnt /mnt/test
func (nsp *Provider) CreateSmbShare(params CreateSmbShareParams) error {
	if len(params.Filesystem) == 0 {
		return fmt.Errorf("CreateSmbShareParams.Filesystem is required")
	}

	return nsp.sendRequest(http.MethodPost, "nas/smb", params)
}

// GetSmbShareName - get share name for filesystem that shared over SMB
func (nsp *Provider) GetSmbShareName(path string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("Filesystem path is required")
	}

	uri := nsp.RestClient.BuildURI(
		fmt.Sprintf("/nas/smb/%s", url.PathEscape(path)),
		map[string]string{"fields": "shareName,shareState"}, //TODO check shareState value?
	)

	response := nefNasSmbResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return "", err
	}

	return response.ShareName, nil
}

// DeleteSmbShare - destroy SMB share by filesystem path
func (nsp *Provider) DeleteSmbShare(path string) error {
	if len(path) == 0 {
		return fmt.Errorf("Filesystem path is empty")
	}

	uri := fmt.Sprintf("/nas/smb/%s", url.PathEscape(path))

	return nsp.sendRequest(http.MethodDelete, uri, nil)
}

// SetFilesystemACL - set filesystem ACL, so NFS share can allow user to write w/o checking UNIX user uid
func (nsp *Provider) SetFilesystemACL(path string, aclRuleSet ACLRuleSet) error {
	if len(path) == 0 {
		return fmt.Errorf("Filesystem path is required")
	}

	uri := fmt.Sprintf("/storage/filesystems/%s/acl", url.PathEscape(path))

	permissions := []string{}
	if aclRuleSet == ACLReadOnly {
		permissions = append(permissions, "read_set")
	} else {
		permissions = append(permissions, "full_set")
	}

	data := &nefStorageFilesystemsACLRequest{
		Type:      "allow",
		Principal: "everyone@",
		Flags: []string{
			"file_inherit",
			"dir_inherit",
		},
		Permissions: permissions,
	}

	return nsp.sendRequest(http.MethodPost, uri, data)
}

// CreateSnapshotParams - params to create snapshot
type CreateSnapshotParams struct {
	// snapshot path w/o leading slash
	Path string `json:"path"`
}

// CreateSnapshot - create snapshot by filesystem path
func (nsp *Provider) CreateSnapshot(params CreateSnapshotParams) error {
	if len(params.Path) == 0 {
		return fmt.Errorf("Parameter 'CreateSnapshotParams.Path' is required")
	}

	return nsp.sendRequest(http.MethodPost, "/storage/snapshots", params)
}

// GetSnapshot - get snapshot by its path
// path - full path to snapshot w/o leading slash (e.g. "p/d/fs@s")
func (nsp *Provider) GetSnapshot(path string) (snapshot Snapshot, err error) {
	if path == "" {
		return snapshot, fmt.Errorf("Snapshot path is empty")
	}

	uri := nsp.RestClient.BuildURI("/storage/snapshots", map[string]string{
		"path":   path,
		"fields": "path,name,parent,creationTime",
	})

	response := nefStorageSnapshotsResponse{}
	err = nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return snapshot, err
	}

	if len(response.Data) == 0 {
		return snapshot, fmt.Errorf("Snapshot '%s' not found", path)
	}

	return response.Data[0], nil
}

// GetSnapshots - get snapshot by its path
func (nsp *Provider) GetSnapshots(volumePath string, recursive bool) ([]Snapshot, error) {
	if len(volumePath) == 0 {
		return []Snapshot{}, fmt.Errorf("Snapshots volume path is empty")
	}

	fields := "path,name,parent,creationTime"

	var params map[string]string
	if recursive {
		params = map[string]string{
			"parent":    volumePath,
			"fields":    fields,
			"recursive": "true",
		}
	} else {
		params = map[string]string{
			"path":   volumePath,
			"fields": fields,
		}
	}

	uri := nsp.RestClient.BuildURI("/storage/snapshots", params)

	response := nefStorageSnapshotsResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return []Snapshot{}, err
	}

	return response.Data, nil
}

// DestroySnapshot - destroy snapshot by path
func (nsp *Provider) DestroySnapshot(path string) error {
	if path == "" {
		return fmt.Errorf("Snapshot path is required")
	}

	uri := fmt.Sprintf("/storage/snapshots/%s", url.PathEscape(path))

	return nsp.sendRequest(http.MethodDelete, uri, nil)
}

// CloneSnapshotParams - params to clone snapshot to filesystem
type CloneSnapshotParams struct {
	// filesystem path w/o leading slash
	TargetPath string `json:"targetPath"`
}

// CloneSnapshot clones snapshot to FS
func (nsp *Provider) CloneSnapshot(path string, params CloneSnapshotParams) error {
	if path == "" {
		return fmt.Errorf("Snapshot path is required")
	}

	if params.TargetPath == "" {
		return fmt.Errorf("Parameter 'CloneSnapshotParams.TargetPath' is required")
	}

	uri := fmt.Sprintf("/storage/snapshots/%s/clone", url.PathEscape(path))

	return nsp.sendRequest(http.MethodPost, uri, params)
}

// GetRSFClusters - get RSF clusters from NS
func (nsp *Provider) GetRSFClusters() ([]RSFCluster, error) {
	uri := nsp.RestClient.BuildURI("/rsf/clusters", map[string]string{
		"fields": "clusterName,nodes",
	})

	response := nefRsfClustersResponse{}
	err := nsp.sendRequestWithStruct(http.MethodGet, uri, nil, &response)
	if err != nil {
		return nil, err
	}

	return response.Data, nil
}

// IsJobDone - check if job is done by jobId
func (nsp *Provider) IsJobDone(jobID string) (bool, error) {
	uri := fmt.Sprintf("/jobStatus/%s", jobID)

	statusCode, bodyBytes, err := nsp.RestClient.Send(http.MethodGet, uri, nil)
	if err != nil { // request failed
		return false, err
	} else if statusCode == http.StatusOK || statusCode == http.StatusCreated { // job is completed
		return true, nil
	} else if statusCode == http.StatusAccepted { // job is in progress (202)
		return false, nil
	}

	// job is failed
	nefError := nsp.parseNefError(bodyBytes, "Job was finished with error")
	if nefError != nil {
		err = nefError
	} else {
		err = fmt.Errorf(
			"Job request returned %d code, but response body doesn't contain explanation: %s",
			statusCode,
			bodyBytes,
		)
	}

	return false, err
}
