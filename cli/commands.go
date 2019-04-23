package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/urfave/cli"
)

// CheckTools checks if the installed tools work correctly
func CheckTools(c *cli.Context) error {
	log.Print("Checking docker...")
	ctx := context.Background()
	docker, err := client.NewEnvClient()
	if err != nil {
		return err
	}
	ping, err := docker.Ping(ctx)

	if err != nil {
		return fmt.Errorf("ERROR: checking docker failed\n%+v", err)
	}
	log.Printf("SUCCESS: Checking docker succeeded (API: v%s)\n", ping.APIVersion)
	return nil
}

// CreateCluster creates a new single-node cluster container and initializes the cluster directory
func CreateCluster(c *cli.Context) error {

	// create cluster network
	networkID, err := createClusterNetwork(c.String("name"))
	if err != nil {
		return err
	}
	log.Printf("Created cluster network with ID %s", networkID)

	if c.IsSet("timeout") && !c.IsSet("wait") {
		return errors.New("Cannot use --timeout flag without --wait flag")
	}

	// environment variables
	env := []string{"K3S_KUBECONFIG_OUTPUT=/output/kubeconfig.yaml"}
	if c.IsSet("env") || c.IsSet("e") {
		env = append(env, c.StringSlice("env")...)
	}
	k3sClusterSecret := ""
	if c.Int("workers") > 0 {
		k3sClusterSecret = fmt.Sprintf("K3S_CLUSTER_SECRET=%s", GenerateRandomString(20))
		env = append(env, k3sClusterSecret)
	}

	// k3s server arguments
	k3sServerArgs := []string{"--https-listen-port", c.String("port")}
	if c.IsSet("server-arg") || c.IsSet("x") {
		k3sServerArgs = append(k3sServerArgs, c.StringSlice("server-arg")...)
	}

	// let's go
	log.Printf("Creating cluster [%s]", c.String("name"))
	dockerID, err := createServer(
		c.GlobalBool("verbose"),
		fmt.Sprintf("docker.io/rancher/k3s:%s", c.String("version")),
		c.String("port"),
		k3sServerArgs,
		env,
		c.String("name"),
		strings.Split(c.String("volume"), ","),
	)
	if err != nil {
		log.Fatalf("ERROR: failed to create cluster\n%+v", err)
	}

	ctx := context.Background()
	docker, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	// wait for k3s to be up and running if we want it
	start := time.Now()
	timeout := time.Duration(c.Int("timeout")) * time.Second
	for c.IsSet("wait") {
		if timeout != 0 && !time.Now().After(start.Add(timeout)) {
			err := DeleteCluster(c)
			if err != nil {
				return err
			}
			return errors.New("Cluster creation exceeded specified timeout")
		}

		out, err := docker.ContainerLogs(ctx, dockerID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
		if err != nil {
			out.Close()
			return fmt.Errorf("ERROR: couldn't get docker logs for %s\n%+v", c.String("name"), err)
		}
		buf := new(bytes.Buffer)
		nRead, _ := buf.ReadFrom(out)
		out.Close()
		output := buf.String()
		if nRead > 0 && strings.Contains(string(output), "Running kubelet") {
			break
		}

		time.Sleep(1 * time.Second)
	}

	createClusterDir(c.String("name"))
	log.Printf("SUCCESS: created cluster [%s]", c.String("name"))
	log.Printf(`You can now use the cluster with:

export KUBECONFIG="$(%s get-kubeconfig --name='%s')"
kubectl cluster-info`, os.Args[0], c.String("name"))

	// worker nodes
	if c.Int("workers") > 0 {
		k3sWorkerArgs := []string{}
		env := []string{k3sClusterSecret}
		log.Printf("Booting %s workers for cluster %s", strconv.Itoa(c.Int("workers")), c.String("name"))
		for i := 0; i < c.Int("workers"); i++ {
			workerID, err := createWorker(
				c.GlobalBool("verbose"),
				fmt.Sprintf("docker.io/rancher/k3s:%s", c.String("version")),
				k3sWorkerArgs,
				env,
				c.String("name"),
				strings.Split(c.String("volume"), ","),
				strconv.Itoa(i),
				c.String("port"),
			)
			if err != nil {
				return fmt.Errorf("ERROR: failed to create worker node for cluster %s\n%+v", c.String("name"), err)
			}
			fmt.Printf("Created worker with ID %s\n", workerID)
		}
	}
	return nil
}

// DeleteCluster removes the cluster container and its cluster directory
func DeleteCluster(c *cli.Context) error {

	// operate on one or all clusters
	clusters := make(map[string]cluster)
	if !c.Bool("all") {
		cluster, err := getCluster(c.String("name"))
		if err != nil {
			return err
		}
		clusters[c.String("name")] = cluster
	} else {
		clusterMap, err := getClusters()
		if err != nil {
			return fmt.Errorf("ERROR: `--all` specified, but no clusters were found\n%+v", err)
		}
		// copy clusterMap
		for k, v := range clusterMap {
			clusters[k] = v
		}
	}

	// remove clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, cluster := range clusters {
		log.Printf("Removing cluster [%s]", cluster.name)
		if len(cluster.workers) > 0 {
			log.Printf("...Removing %d workers\n", len(cluster.workers))
			for _, worker := range cluster.workers {
				if err := removeContainer(worker.ID); err != nil {
					log.Println(err)
					continue
				}
			}
		}
		log.Println("...Removing server")
		deleteClusterDir(cluster.name)
		if err := removeContainer(cluster.server.ID); err != nil {
			return fmt.Errorf("ERROR: Couldn't remove server for cluster %s\n%+v", cluster.name, err)
		}

		if err := deleteClusterNetwork(cluster.name); err != nil {
			log.Printf("WARNING: couldn't delete cluster network for cluster %s\n%+v", cluster.name, err)
		}

		log.Printf("SUCCESS: removed cluster [%s]", cluster.name)
	}

	return nil
}

// StopCluster stops a running cluster container (restartable)
func StopCluster(c *cli.Context) error {

	ctx := context.Background()
	docker, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	clusterNames := []string{}

	// operate on one or all clusters
	if !c.Bool("all") {
		clusterNames = append(clusterNames, c.String("name"))
	} else {
		clusterList, err := getClusterNames()
		if err != nil {
			return fmt.Errorf("ERROR: `--all` specified, but no clusters were found\n%+v", err)
		}
		clusterNames = append(clusterNames, clusterList...)
	}

	// stop clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, name := range clusterNames {
		log.Printf("Stopping cluster [%s]", name)
		cluster, err := getCluster(name)
		if err != nil {
			log.Printf("WARNING: couldn't get docker info for %s", name)
			continue
		}
		if err := docker.ContainerStop(ctx, cluster.server.ID, nil); err != nil {
			fmt.Printf("WARNING: couldn't stop cluster %s\n%+v", cluster.name, err)
			continue
		}
		log.Printf("SUCCESS: stopped cluster [%s]", cluster.name)
	}

	return nil
}

// StartCluster starts a stopped cluster container
func StartCluster(c *cli.Context) error {
	ctx := context.Background()
	docker, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	clusterNames := []string{}

	// operate on one or all clusters
	if !c.Bool("all") {
		clusterNames = append(clusterNames, c.String("name"))
	} else {
		clusterList, err := getClusterNames()
		if err != nil {
			return fmt.Errorf("ERROR: `--all` specified, but no clusters were found\n%+v", err)
		}
		clusterNames = append(clusterNames, clusterList...)
	}

	// stop clusters one by one instead of appending all names to the docker command
	// this allows for more granular error handling and logging
	for _, name := range clusterNames {
		log.Printf("Starting cluster [%s]", name)
		cluster, err := getCluster(name)
		if err != nil {
			log.Printf("WARNING: couldn't get docker info for %s", name)
			continue
		}
		if err := docker.ContainerStart(ctx, cluster.server.ID, types.ContainerStartOptions{}); err != nil {
			fmt.Printf("WARNING: couldn't start cluster %s\n%+v", cluster.name, err)
			continue
		}
		log.Printf("SUCCESS: started cluster [%s]", cluster.name)
	}

	return nil
}

// ListClusters prints a list of created clusters
func ListClusters(c *cli.Context) error {
	printClusters(c.Bool("all"))
	return nil
}

// GetKubeConfig grabs the kubeconfig from the running cluster and prints the path to stdout
func GetKubeConfig(c *cli.Context) error {
	ctx := context.Background()
	docker, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	filters := filters.NewArgs()
	filters.Add("label", "app=k3d")
	filters.Add("label", fmt.Sprintf("cluster=%s", c.String("name")))
	filters.Add("label", "component=server")
	server, err := docker.ContainerList(ctx, types.ContainerListOptions{
		Filters: filters,
	})
	if err != nil {
		return fmt.Errorf("Couldn't get server container for cluster %s\n%+v", c.String("name"), err)
	}

	// get kubeconfig file from container and read contents
	reader, _, err := docker.CopyFromContainer(ctx, server[0].ID, "/output/kubeconfig.yaml")
	if err != nil {
		return fmt.Errorf("ERROR: couldn't copy kubeconfig.yaml from server container %s\n%+v", server[0].ID, err)
	}
	defer reader.Close()

	readBytes, err := ioutil.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("ERROR: couldn't read kubeconfig from container\n%+v", err)
	}

	// create destination kubeconfig file
	clusterDir, err := getClusterDir(c.String("name"))
	destPath := fmt.Sprintf("%s/kubeconfig.yaml", clusterDir)
	if err != nil {
		return err
	}

	kubeconfigfile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("ERROR: couldn't create kubeconfig.yaml in %s\n%+v", clusterDir, err)
	}
	defer kubeconfigfile.Close()

	// write to file, skipping the first 512 bytes which contain file metadata and trimming any NULL characters
	_, err = kubeconfigfile.Write(bytes.Trim(readBytes[512:], "\x00"))
	if err != nil {
		return fmt.Errorf("ERROR: couldn't write to kubeconfig.yaml\n%+v", err)
	}

	// output kubeconfig file path to stdout
	fmt.Println(destPath)

	return nil
}