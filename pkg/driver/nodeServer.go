//TODO Consider to add NodeStageVolume() method:
// - called by k8s to temporarily mount the volume to a staging path
// - staging path is a global directory on the node
// - k8s allows user to use a single volume by multiple pods (for NFS)
// - if all pods run on the same node the single mount point will be used by all of them.

package driver

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/mount"

	"github.com/Nexenta/nexentastor-csi-driver/pkg/arrays"
	"github.com/Nexenta/nexentastor-csi-driver/pkg/config"
	"github.com/Nexenta/nexentastor-csi-driver/pkg/ns"
)

// NodeServer - k8s csi driver node server
type NodeServer struct {
	nodeID     string
	nsResolver *ns.Resolver
	config     *config.Config
	log        *logrus.Entry
}

func (s *NodeServer) refreshConfig() error {
	changed, err := s.config.Refresh()
	if err != nil {
		return err
	}

	if changed {
		s.nsResolver, err = ns.NewResolver(ns.ResolverArgs{
			Address:  s.config.Address,
			Username: s.config.Username,
			Password: s.config.Password,
			Log:      s.log,
		})
		if err != nil {
			return fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
		}
	}

	return nil
}

func (s *NodeServer) resolveNS(datasetPath string) (ns.ProviderInterface, error) {
	nsProvider, err := s.nsResolver.Resolve(datasetPath)
	if err != nil { //TODO check not found error
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"Cannot resolve '%s' on any NexentaStor(s): %s",
			datasetPath,
			err,
		)
	}
	return nsProvider, nil
}

// NodeGetId - get node id
func (s *NodeServer) NodeGetId(ctx context.Context, req *csi.NodeGetIdRequest) (*csi.NodeGetIdResponse, error) {
	s.log.WithField("func", "NodeGetId()").Infof("request: '%+v'", req)

	return &csi.NodeGetIdResponse{
		NodeId: s.nodeID,
	}, nil
}

// NodeGetInfo - get node info
func (s *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	s.log.WithField("func", "NodeGetInfo()").Infof("request: '%+v'", req)

	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}

// NodeGetCapabilities - get node capabilities
func (s *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (
	*csi.NodeGetCapabilitiesResponse,
	error,
) {
	s.log.WithField("func", "NodeGetCapabilities()").Infof("request: '%+v'", req)

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

// NodePublishVolume - mounts NS fs to the node
func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse,
	error,
) {
	l := s.log.WithField("func", "NodePublishVolume()")
	l.Infof("request: '%+v'", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "req.VolumeId must be provided")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "req.TargetPath must be provided")
	}

	//TODO validate VolumeCapability
	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "req.VolumeCapability must be provided")
	}

	// read and validate config
	err := s.refreshConfig()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
	}

	nsProvider, err := s.resolveNS(volumeID)
	if err != nil {
		return nil, err
	}

	l.Infof("resolved NS: %s, %s", nsProvider, volumeID)

	// get NexentaStor filesystem information
	filesystem, err := nsProvider.GetFilesystem(volumeID)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal,
			"Cannot get filesystem '%s' volume: %s",
			volumeID,
			err,
		)
	}

	// volume attributes are passed from ControllerServer.CreateVolume()
	volumeAttributes := req.GetVolumeAttributes()
	if volumeAttributes == nil {
		volumeAttributes = make(map[string]string)
	}

	// get mount options by priority:
	// 	- k8s runtime volume mount options:
	//		- `k8s.PersistentVolume.spec.mountOptions` definition
	// 		- `k8s.StorageClass.mountOptions` (should work in k8s v1.13) //TODO test it
	// 	- runtime volume attributes: `k8s.StorageClass.parameters.mountOptions`
	// 	- driver config file (k8s secret): `defaultMountOptions`
	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	if mountOptions == nil {
		mountOptions = []string{}
	}
	if len(mountOptions) == 0 {
		var configMountOptions string
		if v, ok := volumeAttributes["mountOptions"]; ok && v != "" {
			// `k8s.StorageClass.parameters` in volume definition
			configMountOptions = v
		} else {
			// `defaultMountOptions` in driver config file
			configMountOptions = s.config.DefaultMountOptions
		}
		for _, option := range strings.Split(configMountOptions, ",") {
			if option != "" {
				mountOptions = append(mountOptions, option)
			}
		}
	}

	// add "ro" mount option if k8s requests it
	if req.GetReadonly() {
		//TODO use https://github.com/kubernetes/kubernetes/blob/master/pkg/volume/util/util.go#L759 ?
		mountOptions = arrays.AppendIfRegexpNotExistString(mountOptions, regexp.MustCompile("^ro$"), "ro")
	}

	// get dataIP checking by priority:
	// 	- runtime volume attributes: `k8s.StorageClass.parameters.dataIP`
	// 	- driver config file (k8s secret): `defaultDataIP`
	var dataIP string
	if v, ok := volumeAttributes["dataIP"]; ok && v != "" {
		dataIP = v
	} else {
		dataIP = s.config.DefaultDataIP
	}

	// get mount filesystem type checking by priority:
	// 	- runtime volume attributes: `k8s.StorageClass.parameters.mountFsType`
	// 	- driver config file (k8s secret): `defaultMountFsType`
	// 	- fallback to NFS as default mount filesystem type
	var fsType string
	if v, ok := volumeAttributes["mountFsType"]; ok && v != "" {
		fsType = v
	} else if s.config.DefaultMountFsType != "" {
		fsType = s.config.DefaultMountFsType
	} else {
		fsType = config.FsTypeNFS
	}

	// share and mount filesystem with selected type
	if fsType == config.FsTypeNFS {
		err = s.mountNFS(req, nsProvider, filesystem, dataIP, mountOptions)
	} else if fsType == config.FsTypeCIFS {
		err = s.mountCIFS(req, nsProvider, filesystem, dataIP, mountOptions)
	} else {
		err = status.Errorf(codes.FailedPrecondition, "Unsupported mount filesystem type: '%s'", fsType)
	}
	if err != nil {
		return nil, err
	}

	l.Infof("volume '%s' has been published to '%s'", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) mountNFS(
	req *csi.NodePublishVolumeRequest,
	nsProvider ns.ProviderInterface,
	filesystem ns.Filesystem,
	dataIP string,
	mountOptions []string,
) error {
	// create NFS share if not exists
	if !filesystem.SharedOverNfs {
		err := nsProvider.CreateNfsShare(ns.CreateNfsShareParams{
			Filesystem: filesystem.Path,
		})
		if err != nil {
			return status.Errorf(codes.Internal, "Cannot share filesystem '%s' over NFS: %s", filesystem.Path, err)
		}

		// select read-only or read-write mount options set
		var aclRuleSet ns.ACLRuleSet
		if req.GetReadonly() {
			aclRuleSet = ns.ACLReadOnly
		} else {
			aclRuleSet = ns.ACLReadWrite
		}

		// apply NS filesystem ACL (gets applied only for new volumes, not for already shared pre-provisioned volumes)
		err = nsProvider.SetFilesystemACL(filesystem.Path, aclRuleSet)
		if err != nil {
			return status.Errorf(codes.Internal, "Cannot set filesystem ACL for '%s': %s", filesystem.Path, err)
		}
	}

	// NFS style mount source
	mountSource := fmt.Sprintf("%s:%s", dataIP, filesystem.MountPoint)

	// NFS v3 is used by default if no version specified by user
	mountOptions = arrays.AppendIfRegexpNotExistString(mountOptions, regexp.MustCompile("^vers=.*$"), "vers=3")

	return s.doMount(mountSource, req.GetTargetPath(), config.FsTypeNFS, mountOptions)
}

func (s *NodeServer) mountCIFS(
	req *csi.NodePublishVolumeRequest,
	nsProvider ns.ProviderInterface,
	filesystem ns.Filesystem,
	dataIP string,
	mountOptions []string,
) error {
	// validate CIFS mount options
	for _, optionRE := range []string{"^username=.+$", "^password=.+$"} {
		if len(arrays.FindRegexpIndexesString(mountOptions, regexp.MustCompile(optionRE))) == 0 {
			return status.Errorf(
				codes.FailedPrecondition,
				"Options '%s' must be specified for CIFS mount (got options: %v)",
				optionRE,
				mountOptions,
			)
		}
	}

	// create SMB share if not exists
	if !filesystem.SharedOverSmb {
		err := nsProvider.CreateSmbShare(ns.CreateSmbShareParams{
			Filesystem: filesystem.Path,
			ShareName:  filesystem.GetDefaultSmbShareName(),
		})
		if err != nil {
			return status.Errorf(codes.Internal, "Cannot share filesystem '%s' over SMB: %s", filesystem.Path, err)
		}

		//TODO check if we need ACL rules for SMB
		//TODO apply ACL for specific user?

		// select read-only or read-write mount options set
		var aclRuleSet ns.ACLRuleSet
		if req.GetReadonly() {
			aclRuleSet = ns.ACLReadOnly
		} else {
			aclRuleSet = ns.ACLReadWrite
		}

		// apply NS filesystem ACL (gets applied only for new volumes, not for already shared pre-provisioned volumes)
		err = nsProvider.SetFilesystemACL(filesystem.Path, aclRuleSet)
		if err != nil {
			return status.Errorf(codes.Internal, "Cannot set filesystem ACL for '%s': %s", filesystem.Path, err)
		}
	}

	//get sm share name
	shareName, err := nsProvider.GetSmbShareName(filesystem.Path) //TODO make Filesystem method?
	if err != nil {
		return err
	}

	// CIFS style mount source
	mountSource := fmt.Sprintf("//%s/%s", dataIP, shareName)

	return s.doMount(mountSource, req.GetTargetPath(), config.FsTypeCIFS, mountOptions)
}

// only "nfs" is supported for now
func (s *NodeServer) doMount(mountSource, targetPath, fsType string, mountOptions []string) error {
	l := s.log.WithField("func", "doMount()")

	mounter := mount.New("")

	// check if mountpoint exists, create if there is no such directory
	notMountPoint, err := mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return status.Errorf(
					codes.Internal,
					"Failed to mkdir to share target path '%s': %s",
					targetPath,
					err,
				)
			}
			notMountPoint = true
		} else {
			return status.Errorf(
				codes.Internal,
				"Cannot ensure that target path '%s' can be used as a mount point: %s",
				targetPath,
				err,
			)
		}
	}

	if !notMountPoint { // already mounted
		return status.Errorf(codes.Internal, "Target path '%s' is already a mount point", targetPath)
	}

	l.Infof(
		"mount params: type: '%s', mountSource: '%s', targetPath: '%s', mountOptions(%v): %+v",
		s.config.DefaultMountFsType,
		targetPath,
		mountSource,
		len(mountOptions),
		mountOptions,
	)

	err = mounter.Mount(mountSource, targetPath, fsType, mountOptions)
	if err != nil {
		if os.IsPermission(err) {
			return status.Errorf(
				codes.PermissionDenied,
				"Permission denied to mount '%s' to '%s': %s",
				mountSource,
				targetPath,
				err,
			)
		} else if strings.Contains(err.Error(), "invalid argument") {
			return status.Errorf(
				codes.InvalidArgument,
				"Cannot mount '%s' to '%s', invalid argument: %s",
				mountSource,
				targetPath,
				err,
			)
		}
		return status.Errorf(
			codes.Internal,
			"Failed to mount '%s' to '%s': %s",
			mountSource,
			targetPath,
			err,
		)
	}

	return nil
}

// NodeUnpublishVolume - umount NS fs from the node and delete directory if successful
func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (
	*csi.NodeUnpublishVolumeResponse,
	error,
) {
	l := s.log.WithField("func", "NodeUnpublishVolume()")
	l.Infof("request: '%+v'", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path must be provided")
	}

	mounter := mount.New("")

	if err := mounter.Unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to unmount target path '%s': %s", targetPath, err)
	}

	notMountPoint, err := mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			l.Warnf("mount point '%s' already doesn't exist: '%s', return OK", targetPath, err)
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(
			codes.Internal,
			"Cannot ensure that target path '%s' is a mount point: '%s'",
			targetPath,
			err,
		)
	} else if !notMountPoint { // still mounted
		return nil, status.Errorf(codes.Internal, "Target path '%s' is still mounted", targetPath)
	}

	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "Cannot remove unmounted target path '%s': %s", targetPath, err)
	}

	l.Infof("volume '%s' has been unpublished from '%s'", volumeID, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume - stage volume
//TODO use this to mount NFS, then do bind mount?
func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (
	*csi.NodeStageVolumeResponse,
	error,
) {
	s.log.WithField("func", "NodeStageVolume()").Infof("request: '%+v'", req)
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeUnstageVolume - unstage volume
//TODO use this to umount NFS?
func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (
	*csi.NodeUnstageVolumeResponse,
	error,
) {
	s.log.WithField("func", "NodeUnstageVolume()").Infof("request: '%+v'", req)
	return nil, status.Error(codes.Unimplemented, "")
}

// NewNodeServer - create an instance of node service
func NewNodeServer(driver *Driver) (*NodeServer, error) {
	l := driver.log.WithField("cmp", "NodeServer")
	l.Info("create new NodeServer...")

	nsResolver, err := ns.NewResolver(ns.ResolverArgs{
		Address:  driver.config.Address,
		Username: driver.config.Username,
		Password: driver.config.Password,
		Log:      l,
	})
	if err != nil {
		return nil, fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
	}

	return &NodeServer{
		nodeID:     driver.nodeID,
		nsResolver: nsResolver,
		config:     driver.config,
		log:        l,
	}, nil
}
