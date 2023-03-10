package nvbench

import (
	"github.com/zhyocean/neuvector/share/utils"
)

var DockerNotScored utils.Set = utils.NewSet(
	"1.1.1", "1.1.2",
	"2.15",
	"4.2", "4.3", "4.4", "4.7", "4.8", "4.9", "4.10", "4.11",
	"5.8", "5.17", "5.23", "5.27", "5.29",
	"6.1", "6.2",
	"7.5", "7.8", "7.9", "7.10",
	"8.1.3", "8.1.4",
)

var DockerLevel2 utils.Set = utils.NewSet(
	"1.2.4",
	"2.8", "2.9", "2.10", "2.11", "2.15",
	"4.5", "4.8", "4.11",
	"5.2", "5.22", "5.23", "5.29",
	"7.5", "7.6", "7.8", "7.9", "7.10",
	"8.1.5",
)

var K8SLevel2 utils.Set = utils.NewSet(
	"1.3.6, 2.7, 3.1.1, 3.2.2, 4.2.9, 5.2.9, 5.3.2, 5.4.2, 5.5.1, 5.7.2, 5.7.3, 5.7.4",
)
