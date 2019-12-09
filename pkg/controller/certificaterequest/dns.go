/*
Copyright 2019 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certificaterequest

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/go-logr/logr"

	certmanv1alpha1 "github.com/openshift/certman-operator/pkg/apis/certman/v1alpha1"
)

// AnswerDnsChallenge constructs a fqdn from acmeChallengeSubDomain and domain. An route53 AWS client is then spawned to retrieve HostedZones.
// If zone.HostedZone.Config.PrivateZone is not defined, an attempt to populate this zone will be performed. Only upon success is `fqdn, nil` returned.
func (r *ReconcileCertificateRequest) AnswerDnsChallenge(reqLogger logr.Logger, acmeChallengeToken string, domain string, cr *certmanv1alpha1.CertificateRequest) (fqdn string, err error) {

	fqdn = acmeChallengeSubDomain + "." + domain

	reqLogger.Info(fmt.Sprintf("fqdn acme challenge domain is %v", fqdn))

	dnsClient, err := r.getClient(cr)
	if err != nil {
		reqLogger.Error(err, err.Error())
		return fqdn, err
	}

	hostedZoneOutput, err := dnsClient.ListHostedZones(&route53.ListHostedZonesInput{})
	if err != nil {
		reqLogger.Error(err, err.Error())
		return fqdn, err
	}

	baseDomain := cr.Spec.ACMEDNSDomain

	if string(baseDomain[len(baseDomain)-1]) != "." {
		baseDomain = baseDomain + "."
	}

	for _, hostedzone := range hostedZoneOutput.HostedZones {
		if strings.EqualFold(baseDomain, *hostedzone.Name) {
			zone, err := dnsClient.GetHostedZone(&route53.GetHostedZoneInput{Id: hostedzone.Id})
			if err != nil {
				reqLogger.Error(err, err.Error())
				return fqdn, err
			}

			if !*zone.HostedZone.Config.PrivateZone {
				input := &route53.ChangeResourceRecordSetsInput{
					ChangeBatch: &route53.ChangeBatch{
						Changes: []*route53.Change{
							{
								Action: aws.String(route53.ChangeActionUpsert),
								ResourceRecordSet: &route53.ResourceRecordSet{
									Name: aws.String(fqdn),
									ResourceRecords: []*route53.ResourceRecord{
										{
											Value: aws.String("\"" + acmeChallengeToken + "\""),
										},
									},
									TTL:  aws.Int64(resourceRecordTTL),
									Type: aws.String(route53.RRTypeTxt),
								},
							},
						},
						Comment: aws.String(""),
					},
					HostedZoneId: hostedzone.Id,
				}

				reqLogger.Info(fmt.Sprintf("updating hosted zone %v", hostedzone.Name))

				result, err := dnsClient.ChangeResourceRecordSets(input)
				if err != nil {
					reqLogger.Error(err, result.GoString(), "fqdn", fqdn)
					return fqdn, err
				}

				return fqdn, nil
			}
		}
	}

	return fqdn, errors.New("unknown error prevented from answering DNS challenge")
}

// ValidateDnsWriteAccess spawns a route53 client to retrieve the baseDomain's hostedZoneOutput
// and attempts to write a test TXT ResourceRecord to it. If successful, will return `true, nil`.
func (r *ReconcileCertificateRequest) ValidateDnsWriteAccess(reqLogger logr.Logger, cr *certmanv1alpha1.CertificateRequest) (bool, error) {

	dnsClient, err := r.getClient(cr)
	if err != nil {
		return false, err
	}

	hostedZoneOutput, err := dnsClient.ListHostedZones(&route53.ListHostedZonesInput{})
	if err != nil {
		return false, err
	}

	baseDomain := cr.Spec.ACMEDNSDomain

	if string(baseDomain[len(baseDomain)-1]) != "." {
		baseDomain = baseDomain + "."
	}

	for _, hostedzone := range hostedZoneOutput.HostedZones {
		// Find our specific hostedzone
		if strings.EqualFold(baseDomain, *hostedzone.Name) {

			zone, err := dnsClient.GetHostedZone(&route53.GetHostedZoneInput{Id: hostedzone.Id})
			if err != nil {
				return false, err
			}

			if !*zone.HostedZone.Config.PrivateZone {
				// Build the test record
				input := &route53.ChangeResourceRecordSetsInput{
					ChangeBatch: &route53.ChangeBatch{
						Changes: []*route53.Change{
							{
								Action: aws.String(route53.ChangeActionUpsert),
								ResourceRecordSet: &route53.ResourceRecordSet{
									Name: aws.String("_certman_access_test." + *hostedzone.Name),
									ResourceRecords: []*route53.ResourceRecord{
										{
											Value: aws.String("\"txt_entry\""),
										},
									},
									TTL:  aws.Int64(resourceRecordTTL),
									Type: aws.String(route53.RRTypeTxt),
								},
							},
						},
						Comment: aws.String(""),
					},
					HostedZoneId: hostedzone.Id,
				}

				reqLogger.Info(fmt.Sprintf("updating hosted zone %v", hostedzone.Name))

				// Initiate the Write test
				_, err := dnsClient.ChangeResourceRecordSets(input)
				if err != nil {
					return false, err
				}

				// After successfull write test clean up the test record and test deletion of that record.
				input.ChangeBatch.Changes[0].Action = aws.String(route53.ChangeActionDelete)
				_, err = dnsClient.ChangeResourceRecordSets(input)
				if err != nil {
					reqLogger.Error(err, "Error while deleting Write Access record")
					return false, err
				}
				// If Write and Delete are successfull return clean.
				return true, nil
			}
		}
	}

	return false, nil
}

// DeleteAcmeChallengeResourceRecords spawns an AWS client, constructs baseDomain to retrieve the HostedZones. The ResourceRecordSets are
// then requested, if returned and validated, the record is updated to an empty struct to remove the ACME challange.
func (r *ReconcileCertificateRequest) DeleteAcmeChallengeResourceRecords(reqLogger logr.Logger, cr *certmanv1alpha1.CertificateRequest) error {

	dnsClient, err := r.getClient(cr)
	if err != nil {
		return err
	}

	hostedZoneOutput, err := dnsClient.ListHostedZones(&route53.ListHostedZonesInput{})
	if err != nil {
		return err
	}

	baseDomain := cr.Spec.ACMEDNSDomain

	if string(baseDomain[len(baseDomain)-1]) != "." {
		baseDomain = baseDomain + "."
	}

	for _, hostedzone := range hostedZoneOutput.HostedZones {
		if strings.EqualFold(baseDomain, *hostedzone.Name) {
			zone, err := dnsClient.GetHostedZone(&route53.GetHostedZoneInput{Id: hostedzone.Id})
			if err != nil {
				return err
			}

			if !*zone.HostedZone.Config.PrivateZone {

				for _, domain := range cr.Spec.DnsNames {
					// Format domain strings, no leading '*', must lead with '.'
					domain = strings.TrimPrefix(domain, "*")
					if !strings.HasPrefix(domain, ".") {
						domain = "." + domain
					}
					fqdn := acmeChallengeSubDomain + domain
					fqdnWithDot := fqdn + "."

					reqLogger.Info(fmt.Sprintf("deleting resource record %v", fqdn))

					resp, err := dnsClient.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{
						HostedZoneId:    aws.String(*hostedzone.Id), // Required
						StartRecordName: aws.String(fqdn),
						StartRecordType: aws.String(route53.RRTypeTxt),
					})

					if err != nil {
						return err
					}
					if len(resp.ResourceRecordSets) > 0 &&
						*resp.ResourceRecordSets[0].Name == fqdnWithDot &&
						*resp.ResourceRecordSets[0].Type == route53.RRTypeTxt &&
						len(resp.ResourceRecordSets[0].ResourceRecords) > 0 {
						for _, rr := range resp.ResourceRecordSets[0].ResourceRecords {
							input := &route53.ChangeResourceRecordSetsInput{
								ChangeBatch: &route53.ChangeBatch{
									Changes: []*route53.Change{
										{
											Action: aws.String(route53.ChangeActionDelete),
											ResourceRecordSet: &route53.ResourceRecordSet{
												Name: aws.String(fqdn),
												ResourceRecords: []*route53.ResourceRecord{
													{
														Value: aws.String(*rr.Value),
													},
												},
												TTL:  aws.Int64(resourceRecordTTL),
												Type: aws.String(route53.RRTypeTxt),
											},
										},
									},
									Comment: aws.String(""),
								},
								HostedZoneId: hostedzone.Id,
							}

							reqLogger.Info(fmt.Sprintf("updating hosted zone %v", hostedzone.Name))

							result, err := dnsClient.ChangeResourceRecordSets(input)
							if err != nil {
								reqLogger.Error(err, result.GoString())
								return nil
							}
						}
					}
				}
			}
		}
	}

	return nil
}

// DeleteAllAcmeChallengeResourceRecords to delete all records in a hosted zone that begin with the prefix defined by the const acmeChallengeSubDomain
func (r *ReconcileCertificateRequest) DeleteAllAcmeChallengeResourceRecords(reqLogger logr.Logger, cr *certmanv1alpha1.CertificateRequest) error {
	// This function is for record clean up. If we are unable to find the records to delete them we silently accept these errors
	// without raising an error. If the record was already deleted that's fine.

	dnsClient, err := r.getClient(cr)
	if err != nil {
		return err
	}

	// Make sure that the domain ends with a dot.
	baseDomain := cr.Spec.ACMEDNSDomain
	if string(baseDomain[len(baseDomain)-1]) != "." {
		baseDomain = baseDomain + "."
	}

	// Calls function to get the hostedzone of the domain of our CertificateRequest
	hostedzone, err := dnsClient.SearchForHostedZone(baseDomain)
	if err != nil {
		reqLogger.Error(err, "Unable to find appropriate hostedzone.")
		return err
	}

	// Get a list of RecordSets from our hostedzone that match our search criteria
	// Criteria - record name starts with our acmechallenge prefix, record is a TXT type
	listRecordSets, err := dnsClient.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(*hostedzone.Id), // Required
		StartRecordName: aws.String(acmeChallengeSubDomain + "*"),
		StartRecordType: aws.String(route53.RRTypeTxt),
	})
	if err != nil {
		reqLogger.Error(err, "Unable to retrieve acme records for hostedzone.")
		return err
	}

	// Construct an Input object and populate it with records we intend to change
	// In this case we're adding all acme challenge records found above and setting their action to Delete
	input := dnsClient.BuildR53Input(*hostedzone.Id)
	for _, record := range listRecordSets.ResourceRecordSets {
		if strings.Contains(*record.Name, acmeChallengeSubDomain) {
			change, err := dnsClient.CreateR53TXTRecordChange(record.Name, route53.ChangeActionDelete, record.ResourceRecords[0].Value)
			if err != nil {
				reqLogger.Error(err, "Error creating record change object")
			}
			input.ChangeBatch.Changes = append(input.ChangeBatch.Changes, &change)
		}
	}

	// Sent the completed Input object to Route53 to delete the acme records
	result, err := dnsClient.ChangeResourceRecordSets(input)
	if err != nil {
		reqLogger.Error(err, result.GoString())
		return nil
	}

	return nil
}

// func newTXTRecordSet(fqdn, value string, ttl int) *route53.ResourceRecordSet {
// 	return &route53.ResourceRecordSet{
// 		Name: aws.String(fqdn),
// 		Type: aws.String(route53.RRTypeTxt),
// 		TTL:  aws.Int64(int64(ttl)),
// 		ResourceRecords: []*route53.ResourceRecord{
// 			{Value: aws.String(value)},
// 		},
// 	}
// }
