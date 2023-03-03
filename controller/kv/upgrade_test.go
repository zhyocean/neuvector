package kv

import (
	//	"fmt"
	"os"
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/zhyocean/neuvector/share"
	"github.com/zhyocean/neuvector/share/utils"
)

func preTestDebug() {
	log.SetOutput(os.Stdout)
	log.SetFormatter(&utils.LogFormatter{Module: "TEST"})
	log.SetLevel(log.DebugLevel)
}

func preTest() {
	log.SetOutput(os.Stdout)
	log.SetFormatter(&utils.LogFormatter{Module: "TEST"})
	log.SetLevel(log.FatalLevel)
}

func postTest() {
	log.SetLevel(log.DebugLevel)
}

func TestConvertRoleGroupsToGroupRoleDomains(t *testing.T) {
	preTest()

	{
		roleGroups := map[string][]string{
			"role-2": []string{"g5", "g4"},
			"admin":  []string{"g95", "g94"},
			"role-1": []string{"g2", "g1", "g3"},
			"reader": []string{"g23"},
			"role-3": []string{"g6"},
		}
		if groupRoleMappings, err := ConvertRoleGroupsToGroupRoleDomains(roleGroups); err != nil {
			t.Errorf("success expected but failed, err=%v", err)
		} else {
			expects := []*share.GroupRoleMapping{
				&share.GroupRoleMapping{
					Group:      "g94",
					GlobalRole: "admin",
				},
				&share.GroupRoleMapping{
					Group:      "g95",
					GlobalRole: "admin",
				},
				&share.GroupRoleMapping{
					Group:      "g23",
					GlobalRole: "reader",
				},
				&share.GroupRoleMapping{
					Group:      "g1",
					GlobalRole: "role-1",
				},
				&share.GroupRoleMapping{
					Group:      "g2",
					GlobalRole: "role-1",
				},
				&share.GroupRoleMapping{
					Group:      "g3",
					GlobalRole: "role-1",
				},
				&share.GroupRoleMapping{
					Group:      "g4",
					GlobalRole: "role-2",
				},
				&share.GroupRoleMapping{
					Group:      "g5",
					GlobalRole: "role-2",
				},
				&share.GroupRoleMapping{
					Group:      "g6",
					GlobalRole: "role-3",
				},
			}
			if len(expects) != len(groupRoleMappings) {
				t.Errorf("result len=%v, expect len=%v", len(groupRoleMappings), len(expects))
			} else {
				for idx, groupRoleMapping := range groupRoleMappings {
					expect := expects[idx]
					if groupRoleMapping.Group != expect.Group {
						t.Errorf("[%d] result group=%v, expect group=%v", idx, groupRoleMapping.Group, expect.Group)
					} else if groupRoleMapping.GlobalRole != expect.GlobalRole {
						t.Errorf("[%d] result group global role=%v, expect group global role=%v", idx, groupRoleMapping.GlobalRole, expect.GlobalRole)
					}
				}
			}
		}
	}
	{
		roleGroups := map[string][]string{
			"role-1": []string{"g1", "g2", "g3"},
			"role-2": []string{"g4", "g5"},
			"role-3": []string{"g3"}, // multiple roles for "g3". should fail
		}
		if _, err := ConvertRoleGroupsToGroupRoleDomains(roleGroups); err == nil {
			t.Errorf("failed expected but succeeded")
		}
	}

	postTest()
}
