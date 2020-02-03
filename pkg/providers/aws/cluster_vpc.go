package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"
	"github.com/sirupsen/logrus"
	"reflect"
	"regexp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/util/wait"
	"time"

	errorUtil "github.com/pkg/errors"
)

const (
	defaultSubnetPostfix        = "subnet-group"
	defaultSecurityGroupPostfix = "security-group"
)

// ensures a subnet group is in place for the creation of a resource
func SetupSecurityGroup(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) error {
	logrus.Info("setting resource security group")
	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, c)
	if err != nil {
		return errorUtil.Wrap(err, "error getting cluster id")
	}

	// build security group name
	secName, err := BuildInfraName(ctx, c, defaultSecurityGroupPostfix, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrap(err, "error building subnet group name")
	}

	// get cluster cidr group
	vpcID, cidr, err := GetCidr(ctx, c, ec2Svc)
	if err != nil {
		return errorUtil.Wrap(err, "error finding cidr block")
	}

	foundSecGroup, err := getSecurityGroup(ec2Svc, secName)
	if err != nil {
		return errorUtil.Wrap(err, "error get security group")
	}

	if foundSecGroup == nil {
		// create security group
		logrus.Info(fmt.Sprintf("creating security group from cluster %s", clusterID))
		if _, err := ec2Svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
			Description: aws.String(fmt.Sprintf("security group for cluster %s", clusterID)),
			GroupName:   aws.String(secName),
			VpcId:       aws.String(vpcID),
		}); err != nil {
			return errorUtil.Wrap(err, "error creating security group")
		}
		return nil
	}

	// build ip permission
	ipPermission := &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges: []*ec2.IpRange{
			{
				CidrIp: aws.String(cidr),
			},
		},
	}

	// check if correct permissions are in place
	for _, perm := range foundSecGroup.IpPermissions {
		if reflect.DeepEqual(perm, ipPermission) {
			logrus.Info("ip permissions are correct for postgres resource")
			return nil
		}
	}

	// authorize ingress
	logrus.Info("setting ingress ip permissions")
	if _, err := ec2Svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(*foundSecGroup.GroupId),
		IpPermissions: []*ec2.IpPermission{
			ipPermission,
		},
	}); err != nil {
		return errorUtil.Wrap(err, "error authorizing security group ingress")
	}

	return nil
}

// GetVPCSubnets returns a list of subnets associated with cluster VPC
func GetVPCSubnets(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) ([]*ec2.Subnet, error) {
	logrus.Info("gathering cluster vpc and subnet information")

	// poll subnets to ensure credentials have reconciled
	subs, err := getSubnets(ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting subnets")
	}

	// get cluster vpc
	foundVPC, err := getVpc(ctx, c, ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpcs")
	}

	// check if found cluster vpc
	if foundVPC == nil {
		return nil, errorUtil.New("error, unable to find a vpc")
	}

	// find associated subnets
	var associatedSubs []*ec2.Subnet
	for _, sub := range subs {
		if *sub.VpcId == *foundVPC.VpcId {
			associatedSubs = append(associatedSubs, sub)
		}
	}

	// check if found subnets associated with cluster vpc
	if associatedSubs == nil {
		return nil, errorUtil.New("error, unable to find subnets associated with cluster vpc")
	}

	return associatedSubs, nil
}

// GetSubnetIDS returns a list of subnet ids associated with cluster vpc
func GetAllSubnetIDS(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) ([]*string, error) {
	logrus.Info("gathering all vpc subnets")
	subs, err := GetVPCSubnets(ctx, c, ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpc subnets")
	}

	// build list of subnet ids
	var subIDs []*string
	for _, sub := range subs {
		subIDs = append(subIDs, sub.SubnetId)
	}

	if subIDs == nil {
		return nil, errorUtil.New("failed to get list of subnet ids")
	}

	return subIDs, nil
}

// GetSubnetIDS returns a list of subnet ids associated with cluster vpc
func GetPrivateSubnetIDS(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) ([]*string, error) {
	logrus.Info("gathering private vpc subnets")
	subs, err := GetVPCSubnets(ctx, c, ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpc subnets")
	}

	regexpStr := "\\b(\\w*private\\w*)\\b"
	anReg, err := regexp.Compile(regexpStr)
	if err != nil {
		return nil, errorUtil.Wrapf(err, "failed to compile regexp %s", regexpStr)
	}

	var privateSubs []*ec2.Subnet
	for _, sub := range subs {
		for _, tags := range sub.Tags {
			if anReg.MatchString(*tags.Value) {
				privateSubs = append(privateSubs, sub)
			}
		}
	}

	// build list of subnet ids
	var subIDs []*string
	for _, sub := range privateSubs {
		subIDs = append(subIDs, sub.SubnetId)
	}

	if subIDs == nil {
		return nil, errorUtil.New("failed to get list of private subnet ids")
	}

	return subIDs, nil
}

// returns vpc id and cidr block for found vpc
func GetCidr(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) (string, string, error) {
	logrus.Info("gathering cidr block for cluster")
	foundVPC, err := getVpc(ctx, c, ec2Svc)
	if err != nil {
		return "", "", errorUtil.Wrap(err, "error getting vpcs")
	}

	// check if found cluster vpc
	if foundVPC == nil {
		return "", "", errorUtil.New("error, unable to find a vpc")
	}

	return *foundVPC.VpcId, *foundVPC.CidrBlock, nil
}

// function to get subnets, used to check/wait on AWS credentials
func getSubnets(ec2Svc ec2iface.EC2API) ([]*ec2.Subnet, error) {
	var subs []*ec2.Subnet
	err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		listOutput, err := ec2Svc.DescribeSubnets(&ec2.DescribeSubnetsInput{})
		if err != nil {
			return false, nil
		}
		subs = listOutput.Subnets
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return subs, nil
}

// function to get vpc of a cluster
func getVpc(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API) (*ec2.Vpc, error) {
	logrus.Info("finding cluster vpc")
	// get vpcs
	vpcs, err := ec2Svc.DescribeVpcs(&ec2.DescribeVpcsInput{})
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting subnets")
	}

	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, c)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting clusterID")
	}

	// find associated vpc to cluster
	var foundVPC *ec2.Vpc
	for _, vpc := range vpcs.Vpcs {
		for _, tag := range vpc.Tags {
			if *tag.Value == fmt.Sprintf("%s-vpc", clusterID) {
				foundVPC = vpc
			}
		}
	}

	if foundVPC == nil {
		return nil, errorUtil.New("error, no vpc found")
	}

	return foundVPC, nil
}

func getSecurityGroup(ec2Svc ec2iface.EC2API, secName string) (*ec2.SecurityGroup, error) {
	// get security groups
	secGroups, err := ec2Svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{})
	if err != nil {
		return nil, errorUtil.Wrap(err, "failed to return information about security groups")
	}

	// check if security group exists
	var foundSecGroup *ec2.SecurityGroup
	for _, sec := range secGroups.SecurityGroups {
		if *sec.GroupName == secName {
			foundSecGroup = sec
			break
		}
	}

	return foundSecGroup, nil
}
