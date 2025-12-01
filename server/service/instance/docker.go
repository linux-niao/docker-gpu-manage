package instance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flipped-aurora/gin-vue-admin/server/global"
	"github.com/flipped-aurora/gin-vue-admin/server/model/computenode"
	"github.com/flipped-aurora/gin-vue-admin/server/model/imageregistry"
	instanceModel "github.com/flipped-aurora/gin-vue-admin/server/model/instance"
	"github.com/flipped-aurora/gin-vue-admin/server/model/product"
	"go.uber.org/zap"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// DockerService Docker服务
type DockerService struct{}

// ContainerConfig 容器配置
type ContainerConfig struct {
	Image        string // 镜像地址
	Name         string // 容器名称
	CPUCores     int64  // CPU核心数
	MemoryGB     int64  // 内存大小(GB)
	SystemDiskGB int64  // 系统盘大小(GB)
	DataDiskGB   int64  // 数据盘大小(GB)
	GPUCount     int64  // GPU数量
}

// CreateDockerClient 创建Docker客户端
func (d *DockerService) CreateDockerClient(node *computenode.ComputeNode) (*client.Client, error) {
	if node.DockerAddress == nil || *node.DockerAddress == "" {
		return nil, fmt.Errorf("节点Docker连接地址为空")
	}

	dockerHost := *node.DockerAddress

	// 检查是否使用TLS
	useTLS := node.UseTls != nil && *node.UseTls

	if useTLS {
		// 使用TLS连接
		if node.CaCert == nil || node.ClientCert == nil || node.ClientKey == nil {
			return nil, fmt.Errorf("TLS证书配置不完整")
		}

		// 创建TLS配置
		tlsConfig, err := d.createTLSConfig(*node.CaCert, *node.ClientCert, *node.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("创建TLS配置失败: %v", err)
		}

		// 创建HTTP客户端
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
			Timeout: 30 * time.Second,
		}

		return client.NewClientWithOpts(
			client.WithHost(dockerHost),
			client.WithHTTPClient(httpClient),
			client.WithAPIVersionNegotiation(),
		)
	}

	// 不使用TLS
	return client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
}

// createTLSConfig 创建TLS配置
func (d *DockerService) createTLSConfig(caCert, clientCert, clientKey string) (*tls.Config, error) {
	// 加载CA证书
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM([]byte(caCert)) {
		return nil, fmt.Errorf("无法解析CA证书")
	}

	// 加载客户端证书
	cert, err := tls.X509KeyPair([]byte(clientCert), []byte(clientKey))
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书失败: %v", err)
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// CreateContainer 创建容器
func (d *DockerService) CreateContainer(ctx context.Context, node *computenode.ComputeNode, config *ContainerConfig) (containerID string, err error) {
	// 创建Docker客户端
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 构建容器配置
	containerConfig := &container.Config{
		Image: config.Image,
		Labels: map[string]string{
			"managed-by": "docker-gpu-manage",
			"instance":   config.Name,
		},
	}

	// 构建主机配置
	hostConfig := &container.HostConfig{}

	// CPU配置: --cpus=N
	if config.CPUCores > 0 {
		// NanoCPUs 是以纳秒为单位的CPU配额，1核 = 1e9 纳秒
		hostConfig.NanoCPUs = config.CPUCores * 1e9
	}

	// 内存配置: --memory=Ng
	if config.MemoryGB > 0 {
		// Memory 是以字节为单位
		hostConfig.Memory = config.MemoryGB * 1024 * 1024 * 1024
	}

	// 数据盘配置: 创建命名卷并挂载到 /data
	if config.DataDiskGB > 0 {
		volumeName := fmt.Sprintf("%s-data", config.Name)

		// 创建数据卷
		_, err = cli.VolumeCreate(ctx, volume.CreateOptions{
			Name: volumeName,
			Labels: map[string]string{
				"managed-by": "docker-gpu-manage",
				"instance":   config.Name,
			},
		})
		if err != nil {
			global.GVA_LOG.Warn("创建数据卷失败，可能已存在", zap.Error(err))
		}

		// 挂载数据卷到 /data
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: volumeName,
			Target: "/data",
		})
	}

	// GPU配置: --gpus N
	if config.GPUCount > 0 {
		hostConfig.DeviceRequests = []container.DeviceRequest{
			{
				Driver: "nvidia",
				Count:  int(config.GPUCount),
				Capabilities: [][]string{
					{"gpu"},
				},
			},
		}
	}

	// 尝试创建容器（先尝试带系统盘限制）
	var resp container.CreateResponse

	resp, err = cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, config.Name)

	if err != nil {
		return "", fmt.Errorf("创建容器失败: %v", err)
	}

	// 启动容器
	if err = cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// 启动失败，删除已创建的容器
		_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("启动容器失败: %v", err)
	}

	return resp.ID, nil
}

// DeleteContainer 删除容器及其数据卷
func (d *DockerService) DeleteContainer(ctx context.Context, node *computenode.ComputeNode, containerID string, containerName string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	// 先停止容器
	timeout := 10
	_ = cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})

	// 删除容器
	err = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true, // 同时删除匿名卷
	})
	if err != nil {
		global.GVA_LOG.Warn("删除容器失败", zap.Error(err))
	}

	// 删除命名数据卷
	volumeName := fmt.Sprintf("%s-data", containerName)
	err = cli.VolumeRemove(ctx, volumeName, true)
	if err != nil {
		global.GVA_LOG.Warn("删除数据卷失败", zap.Error(err))
	}

	return nil
}

// StopContainer 停止容器
func (d *DockerService) StopContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	timeout := 30
	return cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer 启动容器
func (d *DockerService) StartContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	return cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// RestartContainer 重启容器
func (d *DockerService) RestartContainer(ctx context.Context, node *computenode.ComputeNode, containerID string) error {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	timeout := 30
	return cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// GetContainerStatus 获取容器状态
func (d *DockerService) GetContainerStatus(ctx context.Context, node *computenode.ComputeNode, containerID string) (string, error) {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("获取容器信息失败: %v", err)
	}

	return inspect.State.Status, nil
}

// BuildContainerConfig 根据实例信息构建容器配置
func (d *DockerService) BuildContainerConfig(image *imageregistry.ImageRegistry, spec *product.ProductSpec, instanceName string) *ContainerConfig {
	config := &ContainerConfig{
		Name: instanceName,
	}

	// 镜像地址
	if image.Address != nil {
		config.Image = *image.Address
	}

	// CPU核心数
	if spec.CpuCores != nil {
		config.CPUCores = *spec.CpuCores
	}

	// 内存大小
	if spec.MemoryGb != nil {
		config.MemoryGB = *spec.MemoryGb
	}

	// 系统盘大小
	if spec.SystemDiskGb != nil {
		config.SystemDiskGB = *spec.SystemDiskGb
	}

	// 数据盘大小
	if spec.DataDiskGb != nil {
		config.DataDiskGB = *spec.DataDiskGb
	}

	// GPU数量
	if spec.GpuCount != nil {
		config.GPUCount = *spec.GpuCount
	}

	return config
}

// GenerateInstanceName 生成实例名称
func (d *DockerService) GenerateInstanceName(baseName string, instanceID uint) string {
	// 清理名称，只保留字母数字和横杠
	cleanName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, baseName)

	// 移除连续的横杠
	for strings.Contains(cleanName, "--") {
		cleanName = strings.ReplaceAll(cleanName, "--", "-")
	}

	// 移除首尾横杠
	cleanName = strings.Trim(cleanName, "-")

	if cleanName == "" {
		cleanName = "instance"
	}

	return fmt.Sprintf("%s-%d-%d", cleanName, instanceID, time.Now().Unix())
}

// SyncContainerStatus 同步容器状态到数据库
func (d *DockerService) SyncContainerStatus(ctx context.Context, instanceID uint) error {
	var inst instanceModel.Instance
	if err := global.GVA_DB.Where("id = ?", instanceID).First(&inst).Error; err != nil {
		return err
	}

	if inst.ContainerId == nil || *inst.ContainerId == "" || inst.NodeId == nil {
		return nil
	}

	var node computenode.ComputeNode
	if err := global.GVA_DB.Where("id = ?", *inst.NodeId).First(&node).Error; err != nil {
		return err
	}

	status, err := d.GetContainerStatus(ctx, &node, *inst.ContainerId)
	if err != nil {
		status = "unknown"
	}

	return global.GVA_DB.Model(&inst).Update("container_status", status).Error
}

// GetContainerLogs 获取容器日志
func (d *DockerService) GetContainerLogs(ctx context.Context, node *computenode.ComputeNode, containerID string, tail string) (string, error) {
	cli, err := d.CreateDockerClient(node)
	if err != nil {
		return "", fmt.Errorf("创建Docker客户端失败: %v", err)
	}
	defer cli.Close()

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
	}

	logs, err := cli.ContainerLogs(ctx, containerID, options)
	if err != nil {
		return "", fmt.Errorf("获取容器日志失败: %v", err)
	}
	defer logs.Close()

	// 读取日志内容
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(logs)
	if err != nil {
		return "", fmt.Errorf("读取日志内容失败: %v", err)
	}

	return buf.String(), nil
}

// ptr 辅助函数：创建字符串指针
func ptr(s string) *string {
	return &s
}

// int64Ptr 辅助函数：创建int64指针
func int64Ptr(i int64) *int64 {
	return &i
}

// parseInt64 解析字符串为int64
func parseInt64(s string) int64 {
	i, _ := strconv.ParseInt(s, 10, 64)
	return i
}
