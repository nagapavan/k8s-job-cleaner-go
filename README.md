# k8s-job-cleaner-go

Inspired by [dtan4/k8s-job-cleaner](https://github.com/dtan4/k8s-job-cleaner).

Configure cronjab for Kubernetes.  
Example: 
### For standalone K8S cluster

[cronjob.yaml](cronjob.yaml)

### For K8S cluster - Docker For Desktop 

[cronjob.local.yaml](cronjob.yaml)

## Development
You will need [Docker](https://get.docker.com/) to build the image.
```bash
docker build -t <image:tag> .
```
