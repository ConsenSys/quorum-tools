/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package docker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strings"

	"github.com/ethereum/go-ethereum/log"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"

	"github.com/docker/docker/client"

	"gopkg.in/yaml.v2"
)

type Container interface {
	Start() error
	Stop() error
}

type QuorumBuilderConsensus struct {
	Name   string            `yaml:"name"`
	Config map[string]string `yaml:"config"`
}

type QuorumBuilderNodeDocker struct {
	Image  string            `yaml:"image"`
	Config map[string]string `yaml:"config"`
}

type QuorumBuilderNode struct {
	Quorum    QuorumBuilderNodeDocker `yaml:"quorum"`
	TxManager QuorumBuilderNodeDocker `yaml:"tx_manager"`
}

type QuorumBuilder struct {
	Name      string                 `yaml:"name"`
	Genesis   string                 `yaml:"genesis"`
	Consensus QuorumBuilderConsensus `yaml:"consensus"`
	Nodes     []QuorumBuilderNode    `yaml:",flow"`

	commonLabels  map[string]string
	dockerClient  *client.Client
	dockerNetwork *Network
}

func NewQuorumBuilder(r io.Reader) (*QuorumBuilder, error) {
	b := &QuorumBuilder{}
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, b); err != nil {
		return nil, err
	}
	b.dockerClient, err = client.NewEnvClient()
	if err != nil {
		return nil, err
	}
	b.commonLabels = map[string]string{
		"com.quorum.quorum-tools.id": b.Name,
	}
	return b, nil
}

// 1. Build Docker Network
// 2. Start Tx Manager
// 3. Start Quorum
func (qb *QuorumBuilder) Build() error {
	if err := qb.buildDockerNetwork(); err != nil {
		return err
	}
	if err := qb.startTxManagers(); err != nil {
		return err
	}
	return nil
}

func (qb *QuorumBuilder) startTxManagers() error {
	log.Debug("Start Tx Managers")
	return qb.startContainers(func(idx int, node QuorumBuilderNode) (Container, error) {
		if err := qb.pullImage(node.TxManager.Image); err != nil {
			return nil, err
		}
		return NewTesseraTxManager(
			ConfigureNodeIndex(idx),
			ConfigureProvisionId(qb.Name),
			ConfigureDockerClient(qb.dockerClient),
			ConfigureNetwork(qb.dockerNetwork),
			ConfigureDockerImage(node.TxManager.Image),
			ConfigureConfig(node.TxManager.Config),
			ConfigureLabels(qb.commonLabels),
		)
	})
}

func (qb *QuorumBuilder) startQuorums() error {
	return qb.startContainers(func(idx int, node QuorumBuilderNode) (Container, error) {
		if err := qb.pullImage(node.Quorum.Image); err != nil {
			return nil, err
		}
		return NewQuorum(
			ConfigureNodeIndex(idx),
			ConfigureProvisionId(qb.Name),
			ConfigureDockerClient(qb.dockerClient),
			ConfigureNetwork(qb.dockerNetwork),
			ConfigureDockerImage(node.Quorum.Image),
			ConfigureConfig(node.Quorum.Config),
			ConfigureLabels(qb.commonLabels),
		)
	})
}

func (qb *QuorumBuilder) startContainers(containerFn func(idx int, node QuorumBuilderNode) (Container, error)) error {
	readyChan := make(chan struct{})
	errChan := make(chan error)
	for idx, node := range qb.Nodes {
		go func(_idx int, _node QuorumBuilderNode) {
			c, err := containerFn(_idx, _node)
			if err != nil {
				errChan <- fmt.Errorf("container %d: %s", _idx, err)
				return
			}
			log.Debug("Start Container", "idx", _idx)
			if err := c.Start(); err != nil {
				errChan <- fmt.Errorf("container %d: %s", _idx, err)
			} else {
				readyChan <- struct{}{}
			}
		}(idx, node)
	}
	readyCount := 0
	allErr := make([]string, 0)
	for {
		select {
		case <-readyChan:
			readyCount++
		case err := <-errChan:
			allErr = append(allErr, err.Error())
		}
		if len(allErr)+readyCount >= len(qb.Nodes) {
			break
		}
	}
	if len(allErr) > 0 {
		return fmt.Errorf("%d/%d containers are ready\n%s", readyCount, len(qb.Nodes), strings.Join(allErr, "\n"))
	}
	return nil
}

func (qb *QuorumBuilder) buildDockerNetwork() error {
	log.Debug("Create Docker network", "name", qb.Name)
	network, err := NewDockerNetwork(qb.dockerClient, qb.Name, qb.commonLabels)
	if err != nil {
		return err
	}
	qb.dockerNetwork = network
	return nil
}

func (qb *QuorumBuilder) pullImage(image string) error {
	log.Debug("Pull Docker Image", "name", image)
	filters := filters.NewArgs()
	filters.Add("reference", image)

	images, err := qb.dockerClient.ImageList(context.Background(), types.ImageListOptions{
		Filters: filters,
	})

	if len(images) == 0 || err != nil {
		_, err := qb.dockerClient.ImagePull(context.Background(), image, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("pullImage: %s - %s", image, err)
		}
	}
	return nil
}

func (qb *QuorumBuilder) Destroy() error {
	filters := filters.NewArgs()
	for k, v := range qb.commonLabels {
		filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}
	// find all containers
	containers, err := qb.dockerClient.ContainerList(context.Background(), types.ContainerListOptions{Filters: filters})
	if err != nil {
		return fmt.Errorf("destroy: %s", err)
	}
	if err := doWorkInParallel("removing containers", containersToGeneric(containers), func(el interface{}) error {
		c := el.(types.Container)
		log.Debug("removing container", "id", c.ID[:6], "name", c.Names)
		return qb.dockerClient.ContainerRemove(context.Background(), c.ID, types.ContainerRemoveOptions{Force: true})
	}); err != nil {
		return fmt.Errorf("destroy: %s", err)
	}

	// find networks
	networks, err := qb.dockerClient.NetworkList(context.Background(), types.NetworkListOptions{Filters: filters})
	if err != nil {
		return fmt.Errorf("destroy: %s", err)
	}
	if err := doWorkInParallel("removing network", networksToGeneric(networks), func(el interface{}) error {
		c := el.(types.NetworkResource)
		log.Debug("removing network", "id", c.ID[:6], "name", c.Name)
		return qb.dockerClient.NetworkRemove(context.Background(), c.ID)
	}); err != nil {
		return fmt.Errorf("destroy: %s", err)
	}

	return nil
}

func containersToGeneric(n []types.Container) []interface{} {
	g := make([]interface{}, len(n))
	for i := range n {
		g[i] = n[i]
	}
	return g
}

func networksToGeneric(n []types.NetworkResource) []interface{} {
	g := make([]interface{}, len(n))
	for i := range n {
		g[i] = n[i]
	}
	return g
}

func doWorkInParallel(title string, elements []interface{}, callback func(el interface{}) error) error {
	log.Debug(title)
	if len(elements) == 0 {
		return nil
	}
	doneChan := make(chan struct{})
	errChan := make(chan error)
	for _, el := range elements {
		go func(_el interface{}) {
			if err := callback(_el); err != nil {
				errChan <- err
			} else {
				doneChan <- struct{}{}
			}
		}(el)
	}
	doneCount := 0
	allErr := make([]string, 0)
	for {
		select {
		case <-doneChan:
			doneCount++
		case err := <-errChan:
			allErr = append(allErr, err.Error())
		}
		if len(allErr)+doneCount >= len(elements) {
			break
		}
	}
	if len(allErr) > 0 {
		return fmt.Errorf("%s: %d/%d\n%s", title, doneCount, len(elements), strings.Join(allErr, "\n"))
	}
	return nil
}
