package cueeval

#Policy: {
	deployment: {
		name: =~"^api-(dev|staging|production)$"
		replicas: int & >=1 & <=10
		labels: managed_by: "wuko"
	}
	targets: [...{
		region: string & !=""
		service: string & !=""
	}]
	approved?: bool
}

candidate: #Policy & wuko.steps.plan.value

output: candidate & {
	approved: true
}
