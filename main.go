package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/a-h/pager"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"go.uber.org/multierr"
)

var flagDryRun = flag.Bool("dryrun", true, "Set to false to run the deletion.")

func main() {
	flag.Parse()

	err := run(context.Background(), *flagDryRun)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dryRun bool) (err error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		err = fmt.Errorf("unable to load SDK config: %w", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(3)
	var allImages []image
	var inUseImagesECS []inUseImageECS
	var inUseImagesLambda []inUseImageLambda
	var allImagesErr, inUseImagesECSErr, inUseImagesLambdaErr error
	go func() {
		defer wg.Done()
		allImages, allImagesErr = getAllImages(ctx, cfg)
	}()
	go func() {
		defer wg.Done()
		inUseImagesECS, inUseImagesECSErr = getInUseImages(ctx, cfg)
	}()
	go func() {
		defer wg.Done()
		inUseImagesLambda, inUseImagesLambdaErr = getInUseImagesLambda(ctx, cfg)
	}()
	wg.Wait()
	err = multierr.Combine(allImagesErr, inUseImagesECSErr, inUseImagesLambdaErr)
	if err != nil {
		return
	}

	inUseImagesByContainerMap := map[string]struct{}{}
	fmt.Printf("Images in use (ECS):\n")
	for _, img := range inUseImagesECS {
		fmt.Printf("  %v %v\n", img.ServiceName, img.Container)
		inUseImagesByContainerMap[img.Container] = struct{}{}
	}
	fmt.Printf("Images in use (Lambda):\n")
	for _, img := range inUseImagesLambda {
		fmt.Printf("  %v %v\n", img.FunctionName, img.Container)
		inUseImagesByContainerMap[img.Container] = struct{}{}
	}

	repoNames := map[string]struct{}{}
	unusedImagesByRepoName := map[string][]image{}
	var unusedImageCount int
	fmt.Printf("Images that aren't used in ECS:\n")
	for _, img := range allImages {
		if _, ok := inUseImagesByContainerMap[img.URI]; !ok {
			repoNames[img.Repo.Name] = struct{}{}
			unusedImagesByRepoName[img.Repo.Name] = append(unusedImagesByRepoName[img.Repo.Name], img)
			unusedImageCount++
			fmt.Printf("  %v\n", img.URI)
		}
	}

	if !dryRun {
		fmt.Printf("Deleting %d unused images...\n", unusedImageCount)
		for repoName := range repoNames {
			unusedImages := unusedImagesByRepoName[repoName]
			if len(unusedImages) == 0 {
				continue
			}
			fmt.Printf("  %s - deleting %d tags...\n", repoName, len(unusedImages))
			tags := make([]string, len(unusedImages))
			for i := 0; i < len(unusedImages); i++ {
				tags[i] = unusedImages[i].Tag
			}
			// Run 100 tags at a time.
			for tagPage := range pager.New(tags, 100) {
				fmt.Printf("    deleting batch of %d tags...\n", len(tagPage))
				err = deleteImages(ctx, cfg, repoName, tagPage)
				if err != nil {
					err = fmt.Errorf("failed to delete image tags: %w", err)
				}
			}
		}
		fmt.Printf("Deleted %d unused images.\n", unusedImageCount)
	}

	fmt.Println()

	return err
}

func deleteImages(ctx context.Context, cfg aws.Config, repoName string, tags []string) (err error) {
	imageIDs := make([]types.ImageIdentifier, len(tags))
	for i := 0; i < len(tags); i++ {
		imageIDs[i] = types.ImageIdentifier{ImageTag: &tags[i]}
	}

	ecrService := ecr.NewFromConfig(cfg)
	_, err = ecrService.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
		RepositoryName: &repoName,
		ImageIds:       imageIDs,
	})
	return err
}

type image struct {
	Repo repo
	URI  string
	Tag  string
}

func getAllImages(ctx context.Context, cfg aws.Config) (images []image, err error) {
	ecrService := ecr.NewFromConfig(cfg)

	var repositories []repo
	repositories, err = getRepositories(ctx, ecrService)
	if err != nil {
		err = fmt.Errorf("failed to get repositories: %w", err)
		return
	}

	for _, repo := range repositories {
		var tags []string
		tags, err = getRepositoryImages(ctx, ecrService, repo.Name)
		if err != nil {
			err = fmt.Errorf("failed to describe repositories: %w", err)
			return
		}
		for _, tag := range tags {
			images = append(images, image{
				Repo: repo,
				URI:  fmt.Sprintf("%s:%s", repo.URI, tag),
				Tag:  tag,
			})
		}
	}

	return
}

type repo struct {
	URI  string
	Name string
}

func getRepositories(ctx context.Context, svc *ecr.Client) (result []repo, err error) {
	p := ecr.NewDescribeRepositoriesPaginator(svc, &ecr.DescribeRepositoriesInput{})
	for p.HasMorePages() {
		var op *ecr.DescribeRepositoriesOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to describe repositories: %w", err)
			return
		}
		for _, r := range op.Repositories {
			result = append(result, repo{URI: *r.RepositoryUri, Name: *r.RepositoryName})
		}
	}
	return
}

func getRepositoryImages(ctx context.Context, svc *ecr.Client, repositoryName string) (result []string, err error) {
	p := ecr.NewListImagesPaginator(svc, &ecr.ListImagesInput{
		RepositoryName: &repositoryName,
	})
	for p.HasMorePages() {
		var op *ecr.ListImagesOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to list tasks: %w", err)
			return
		}
		for _, id := range op.ImageIds {
			if id.ImageTag != nil {
				result = append(result, *id.ImageTag)
			}
		}
	}
	return
}

type inUseImageECS struct {
	Cluster     string
	ServiceName string
	Container   string
}

func getInUseImages(ctx context.Context, cfg aws.Config) (images []inUseImageECS, err error) {
	ecsService := ecs.NewFromConfig(cfg)

	clusters, err := getClusters(ctx, ecsService)
	if err != nil {
		return
	}

	for _, cluster := range clusters {
		cluster := cluster

		var services []string
		services, err = getClusterServices(ctx, ecsService, cluster)
		if err != nil {
			return
		}

		var serviceNames []string
		for servicesBatch := range pager.New(services, 10) {
			var serviceNameBatch []string
			serviceNameBatch, err = getClusterServiceNames(ctx, ecsService, cluster, servicesBatch)
			if err != nil {
				return
			}
			serviceNames = append(serviceNames, serviceNameBatch...)
		}

		for _, service := range serviceNames {
			var taskARNs []string
			taskARNs, err = getClusterServiceTaskARNs(ctx, ecsService, cluster, service)
			if err != nil {
				return
			}

			for taskARNsBatch := range pager.New(taskARNs, 10) {
				var containerIDsBatch []string
				containerIDsBatch, err = getClusterTaskContainer(ctx, ecsService, cluster, taskARNsBatch)
				if err != nil {
					return
				}
				for _, cnt := range containerIDsBatch {
					images = append(images, inUseImageECS{
						Cluster:     cluster,
						ServiceName: service,
						Container:   cnt,
					})
				}
			}
		}
	}

	return
}

type inUseImageLambda struct {
	FunctionName string
	Container    string
}

func getInUseImagesLambda(ctx context.Context, cfg aws.Config) (inUseImages []inUseImageLambda, err error) {
	lambdaService := lambda.NewFromConfig(cfg)

	p := lambda.NewListFunctionsPaginator(lambdaService, &lambda.ListFunctionsInput{})
	if p.HasMorePages() {
		var op *lambda.ListFunctionsOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to list functions: %w", err)
			return
		}
		for _, f := range op.Functions {
			if string(f.PackageType) != "Image" {
				continue
			}
			var gfo *lambda.GetFunctionOutput
			gfo, err = lambdaService.GetFunction(ctx, &lambda.GetFunctionInput{
				FunctionName: f.FunctionName,
			})
			if err != nil {
				err = fmt.Errorf("failed to get function %q: %w", *f.FunctionName, err)
				return
			}
			inUseImages = append(inUseImages, inUseImageLambda{
				FunctionName: *f.FunctionName,
				Container:    *gfo.Code.ImageUri,
			})
		}
	}
	return
}

func getClusters(ctx context.Context, svc *ecs.Client) (result []string, err error) {
	p := ecs.NewListClustersPaginator(svc, &ecs.ListClustersInput{})
	for p.HasMorePages() {
		var op *ecs.ListClustersOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to list clusters: %w", err)
			return
		}
		result = append(result, op.ClusterArns...)
	}
	return
}

func getClusterServices(ctx context.Context, svc *ecs.Client, cluster string) (result []string, err error) {
	p := ecs.NewListServicesPaginator(svc, &ecs.ListServicesInput{
		Cluster: &cluster,
	})
	for p.HasMorePages() {
		var op *ecs.ListServicesOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to list clusters: %w", err)
			return
		}
		result = append(result, op.ServiceArns...)
	}
	return
}

func getClusterServiceNames(ctx context.Context, svc *ecs.Client, cluster string, services []string) (result []string, err error) {
	op, err := svc.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  &cluster,
		Services: services,
	})
	if err != nil {
		return
	}
	for _, s := range op.Services {
		result = append(result, *s.ServiceName)
	}
	return
}

func getClusterServiceTaskARNs(ctx context.Context, svc *ecs.Client, cluster, service string) (result []string, err error) {
	p := ecs.NewListTasksPaginator(svc, &ecs.ListTasksInput{
		Cluster:     &cluster,
		ServiceName: &service,
	})
	for p.HasMorePages() {
		var op *ecs.ListTasksOutput
		op, err = p.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("failed to list tasks: %w", err)
			return
		}
		result = append(result, op.TaskArns...)
	}
	return
}

func getClusterTaskContainer(ctx context.Context, svc *ecs.Client, cluster string, taskARNs []string) (result []string, err error) {
	dto, err := svc.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Tasks:   taskARNs,
		Cluster: &cluster,
	})
	if err != nil {
		err = fmt.Errorf("failed to get cluster task descriptions: %w", err)
		return
	}
	for _, t := range dto.Tasks {
		for _, c := range t.Containers {
			result = append(result, *c.Image)
		}
	}
	return
}
