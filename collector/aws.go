/*
 * Copyright 2020 RapidLoop, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package collector

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/rapidloop/pgmetrics"
)

type awsCollector struct {
	sess *session.Session
}

func newAwsCollector() (*awsCollector, error) {
	sess, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		return nil, err
	}
	return &awsCollector{sess: sess}, nil
}

func (ac *awsCollector) collect(dbid string, out *pgmetrics.RDS) (err error) {
	// describe the db instance
	rdssvc := rds.New(ac.sess)
	dbinsts, err := rdssvc.DescribeDBInstances(&rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(dbid),
	})
	if err != nil {
		return
	}
	if len(dbinsts.DBInstances) != 1 {
		err = fmt.Errorf("failed to locate database instance %q", dbid)
		return
	}

	// get resource ID and see if enhanced monitoring is enabled
	dbinst := dbinsts.DBInstances[0]
	dbirid := *dbinst.DbiResourceId
	emEnabled := *dbinst.MonitoringInterval > 0

	// list available metrics
	cwsvc := cloudwatch.New(ac.sess)
	avmetrics, err := cwsvc.ListMetrics(&cloudwatch.ListMetricsInput{
		Namespace: aws.String("AWS/RDS"),
		Dimensions: []*cloudwatch.DimensionFilter{
			&cloudwatch.DimensionFilter{
				Name:  aws.String("DBInstanceIdentifier"),
				Value: aws.String(dbid),
			},
		},
	})
	if err != nil {
		err = fmt.Errorf("failed to list CloudWatch metrics: %v", err)
		return
	}

	// form query input
	queries := make([]*cloudwatch.MetricDataQuery, len(avmetrics.Metrics))
	for i, m := range avmetrics.Metrics {
		mn := *m.MetricName
		queries[i] = &cloudwatch.MetricDataQuery{
			Id: aws.String("id" + mn),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Dimensions: []*cloudwatch.Dimension{
						&cloudwatch.Dimension{
							Name:  aws.String("DBInstanceIdentifier"),
							Value: aws.String(dbid),
						},
					},
					MetricName: aws.String(mn),
					Namespace:  aws.String("AWS/RDS"),
				},
				Period: aws.Int64(60),
				Stat:   aws.String("Average"),
			},
		}
	}
	to := time.Now()
	from := to.Add(-5 * time.Minute)
	input := &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(from),
		EndTime:           aws.Time(to),
		ScanBy:            aws.String("TimestampDescending"),
		MetricDataQueries: queries,
	}

	// actually get metrics
	err = cwsvc.GetMetricDataPages(input, func(page *cloudwatch.GetMetricDataOutput, lastPage bool) bool {
		for _, r := range page.MetricDataResults {
			if len(r.Timestamps) >= 1 && len(r.Values) >= 1 {
				id := strings.TrimPrefix(*r.Id, "id")
				val := *r.Values[0]
				if len(out.Basic) == 0 {
					out.Basic = map[string]float64{id: val}
				} else {
					out.Basic[id] = val
				}
			}
		}
		return true
	})
	if err != nil {
		err = fmt.Errorf("failed to get CloudWatch metric data: %v", err)
		return
	}

	// if enhanced monitoring is not enabled, we are done
	if !emEnabled {
		return
	}

	// get last log event
	cwlsvc := cloudwatchlogs.New(ac.sess)
	events, err := cwlsvc.GetLogEvents(&cloudwatchlogs.GetLogEventsInput{
		EndTime:       aws.Int64(time.Now().Unix() * 1000),
		Limit:         aws.Int64(1),
		LogGroupName:  aws.String("RDSOSMetrics"),
		LogStreamName: aws.String(dbirid),
		StartFromHead: aws.Bool(false),
	})
	if err != nil {
		err = fmt.Errorf("failed to get CloudWatchLog events: %v", err)
		return
	}
	if len(events.Events) == 0 || events.Events[0].Message == nil || len(*events.Events[0].Message) == 0 {
		return // didn't find any usable event, ignore
	}
	if err = json.Unmarshal([]byte(*events.Events[0].Message), &out.Enhanced); err != nil {
		err = fmt.Errorf("failed to decode event: %v", err)
		return
	}

	return
}
