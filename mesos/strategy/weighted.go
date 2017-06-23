package strategy

import (
	"github.com/Dataman-Cloud/swan/mesos"
)

type weightedAgent struct {
	agent  *mesos.Agent
	weight float64
}

type weightedAgents []*weightedAgent

func (w weightedAgents) Len() int {
	return len(w)
}

func (w weightedAgents) Swap(i, j int) {
	w[i], w[j] = w[j], w[i]
}

func (w weightedAgents) Less(i, j int) bool {
	return w[i].weight < w[j].weight
}

func weight(agents []*mesos.Agent) weightedAgents {
	weightedList := make([]*weightedAgent, 0)

	for _, agent := range agents {
		cpus, mem, disk, port := agent.Resources()
		weighted := &weightedAgent{
			agent:  agent,
			weight: cpus + mem + disk + float64(port),
		}

		weightedList = append(weightedList, weighted)
	}

	return weightedList
}
