package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
)

var (
	dest     string
	port     int
	region   string
	elbName  string
	sleep    time.Duration
	tags     Tags
	labels   map[string]string
	ec2Attrs map[string]string
)

// TargetGroup is a collection of related hosts that prometheus monitors
type TargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

type Tag struct {
	Key         string
	FilterName  string
	FilterValue string
}
type Tags []Tag

func main() {
	initFlags()

	var elbSvc *elb.ELB

	if elbName != "" {
		elbSvc = elb.New(session.New(), &aws.Config{Region: aws.String(region)})
	}

	filters := []*ec2.Filter{}
	for _, t := range tags {
		filters = append(filters, &ec2.Filter{
			Name:   aws.String(t.FilterName),
			Values: []*string{aws.String(t.FilterValue)},
		})
	}

	e := ec2.New(session.New(), &aws.Config{Region: aws.String(region)})
	params := &ec2.DescribeInstancesInput{Filters: filters}

	for {

		if elbName != "" {
			instanceIds, err := instanceIdsOfELB(elbSvc, elbName)
			if err != nil {
				log.Fatal(err)
				continue
			}

			if len(instanceIds) > 0 {
				params.InstanceIds = instanceIds
			}
		}

		resp, err := e.DescribeInstances(params)
		if err != nil {
			log.Fatal(err)
		}
		instances := flattenReservations(resp.Reservations)

		tagKeys := tags.Keys()
		if len(tagKeys) == 0 {
			tagKeys = allTagKeys(instances)
		}

		targetGroups := groupByTags(instances, tagKeys)
		if len(labels) > 0 {
			for _, tg := range targetGroups {
				for k, v := range labels {
					tg.Labels[k] = v
				}
			}
		}

		b := marshalTargetGroups(targetGroups)
		if dest == "-" {
			_, err = os.Stdout.Write(b)
		} else {
			err = atomicWriteFile(dest, b, ".new")
		}
		if err != nil {
			log.Fatal(err)
		}

		if sleep == 0 {
			break
		} else {
			time.Sleep(sleep)
		}
	}
}

func initFlags() {
	var (
		tagsRaw     string
		regionRaw   string
		labelRaw    string
		ec2AttrsRaw string
	)

	flag.DurationVar(&sleep, "sleep", 0, "Amount of time between regenerating the target_group.json. If 0, terminate after the first generation")
	flag.IntVar(&port, "port", 80, "Port that is exposing /metrics")
	flag.StringVar(&dest, "dest", "-", "File to write the target group JSON. (e.g. `tgroups/target_groups.json`)")
	flag.StringVar(&regionRaw, "region", "us-west-2", "AWS region to query")
	flag.StringVar(&elbName, "elb", "", "AWS ELB AccessPointName")
	flag.StringVar(&tagsRaw, "tags", "Name", "Comma seperated list of tags to group by (e.g. `Environment,Application`). You can also filter by tag value (e.g. `Application,Envionment=Production`)")
	flag.StringVar(&labelRaw, "labels", "", "Comma seperated list of labels. You add custom labels to the targets.(e.g. `Region:A1`)")
	flag.StringVar(&ec2AttrsRaw, "ec2", "", "Comma seperated list of ec2 instance attributes. (e.g. ipAddress) Currently ipAddress is only supported")

	flag.Parse()
	tags = parseTags(tagsRaw)
	region = regionRaw //TODO
	//region = aws.Regions[regionRaw]

	labels = make(map[string]string)
	for _, l := range strings.Split(labelRaw, ",") {
		parts := strings.Split(l, ":")
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}

	ec2Attrs = make(map[string]string)
	for _, i := range strings.Split(ec2AttrsRaw, ",") {
		parts := strings.Split(i, ":")
		if len(parts) == 1 {
			ec2Attrs[parts[0]] = ""
		} else if len(parts) == 2 {
			ec2Attrs[parts[0]] = parts[1]
		}
	}
}

func groupByTags(instances []*ec2.Instance, tags []string) map[string]*TargetGroup {
	targetGroups := make(map[string]*TargetGroup)

	for _, instance := range instances {
		if *instance.State.Code != 16 { // 16 = Running
			continue
		}

		key := ""
		for _, tagKey := range tags {
			key = fmt.Sprintf("%s|%s=%s", key, tagKey, getTag(instance, tagKey))
		}
		for k, v := range ec2Attrs {
			attrKey := v
			if attrKey == "" {
				attrKey = k
			}
			key = fmt.Sprintf("%s|%s=%s", key, attrKey, getInstanceAttribute(instance, k))
		}

		targetGroup, ok := targetGroups[key]
		if !ok {
			labels := make(map[string]string)
			for _, tagKey := range tags {
				tagVal := getTag(instance, tagKey)
				if tagVal != "" {
					labels[tagKey] = tagVal
				}
			}
			for k, v := range ec2Attrs {
				tagVal := getInstanceAttribute(instance, k)
				if tagVal != "" {
					if v != "" {
						labels[v] = tagVal
					} else if k != "" {
						labels[k] = tagVal
					}
				}
			}
			targetGroup = &TargetGroup{
				Labels:  labels,
				Targets: make([]string, 0),
			}
			targetGroups[key] = targetGroup
		}

		target := fmt.Sprintf("%s:%d", *instance.PrivateIpAddress, port)
		targetGroup.Targets = append(targetGroup.Targets, target)
	}

	return targetGroups
}

func marshalTargetGroups(targetGroups map[string]*TargetGroup) []byte {
	// We need to transform targetGroups into a values list sorted by key
	tgList := []*TargetGroup{}
	keys := []string{}
	for k, _ := range targetGroups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tgList = append(tgList, targetGroups[k])
	}

	b, err := json.MarshalIndent(tgList, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	return b
}

func atomicWriteFile(filename string, data []byte, tmpSuffix string) error {
	err := ioutil.WriteFile(filename+tmpSuffix, data, 0644)
	if err != nil {
		return err
	}
	err = os.Rename(filename+tmpSuffix, filename)
	if err != nil {
		return err
	}
	return nil
}

func getTag(instance *ec2.Instance, key string) string {
	for _, t := range instance.Tags {
		if *t.Key == key {
			return *t.Value
		}
	}
	return ""
}

func flattenReservations(reservations []*ec2.Reservation) []*ec2.Instance {
	instances := make([]*ec2.Instance, 0)
	for _, r := range reservations {
		instances = append(instances, r.Instances...)
	}
	return instances
}

func parseTags(tagsRaw string) Tags {
	fields := strings.Split(tagsRaw, ",")
	if fields[0] == "" && len(fields) == 1 {
		return Tags{}
	}
	tags := make(Tags, len(fields))
	for i, t := range fields {
		parts := strings.Split(t, "=")
		switch len(parts) {
		case 1:
			tags[i] = Tag{
				Key:         t,
				FilterName:  "tag-key",
				FilterValue: t,
			}
		case 2:
			tags[i] = Tag{
				Key:         parts[0],
				FilterName:  "tag:" + parts[0],
				FilterValue: parts[1],
			}
		default:
			log.Fatalf("Unrecognized tag filter %v", t)
		}
	}
	return tags
}

func (tags Tags) Keys() []string {
	seen := map[string]bool{}
	keys := []string{}
	for _, t := range tags {
		if !seen[t.Key] {
			seen[t.Key] = true
			keys = append(keys, t.Key)
		}
	}
	return keys
}

func allTagKeys(instances []*ec2.Instance) []string {
	tagSet := map[string]struct{}{}
	for _, instance := range instances {
		for _, t := range instance.Tags {
			tagSet[*t.Key] = struct{}{}
		}
	}
	tags := []string{}
	for tag, _ := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func instanceIdsOfELB(elbSvc *elb.ELB, name string) ([]*string, error) {
	var instanceIds []*string

	params := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{
			aws.String(elbName),
		},
		PageSize: aws.Int64(1),
	}
	resp, err := elbSvc.DescribeLoadBalancers(params)
	if err != nil {
		return nil, err
	}

	for _, d := range resp.LoadBalancerDescriptions {
		for _, i := range d.Instances {
			instanceIds = append(instanceIds, i.InstanceId)
		}
	}

	return instanceIds, nil
}

func getInstanceAttribute(instance *ec2.Instance, name string) string {
	switch name {
	case "ipAddress":
		if instance.PublicIpAddress != nil {
			return *instance.PublicIpAddress
		}
	case "privateIpAddress":
		if instance.PrivateIpAddress != nil {
			return *instance.PrivateIpAddress
		}
	case "availabilityZone":
		if instance.Placement != nil {
			return *instance.Placement.AvailabilityZone
		}
	case "vpcId":
		if instance.VpcId != nil {
			return *instance.VpcId
		}
	}
	return ""
}
