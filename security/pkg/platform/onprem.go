// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package platform

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"istio.io/istio/security/pkg/pki"
)

// OnPremClientImpl is the implementation of on premise metadata client.
type OnPremClientImpl struct {
	certFile string
}

// NewOnPremClientImpl creates a new OnPremClientImpl.
func NewOnPremClientImpl(certChainFile string) *OnPremClientImpl {
	return &OnPremClientImpl{certChainFile}
}

// GetDialOptions returns the GRPC dial options to connect to the CA.
func (ci *OnPremClientImpl) GetDialOptions(cfg *ClientConfig) ([]grpc.DialOption, error) {
	transportCreds, err := getTLSCredentials(cfg.CertChainFile, cfg.KeyFile, cfg.RootCACertFile)
	if err != nil {
		return nil, err
	}

	var options []grpc.DialOption
	options = append(options, grpc.WithTransportCredentials(transportCreds))
	return options, nil
}

// IsProperPlatform returns whether the platform is on premise.
func (ci *OnPremClientImpl) IsProperPlatform() bool {
	return true
}

// GetServiceIdentity gets the service account from the cert SAN field.
func (ci *OnPremClientImpl) GetServiceIdentity() (string, error) {
	certBytes, err := ioutil.ReadFile(ci.certFile)
	if err != nil {
		return "", err
	}
	cert, err := pki.ParsePemEncodedCertificate(certBytes)
	if err != nil {
		return "", err
	}
	serviceIDs, err := pki.ExtractIDs(cert.Extensions)
	if err != nil {
		return "", err
	}
	if len(serviceIDs) != 1 {
		return "", fmt.Errorf("Cert has %v SAN fields, should be 1", len(serviceIDs))
	}
	return serviceIDs[0], nil
}

// GetAgentCredential passes the certificate to control plane to authenticate
func (ci *OnPremClientImpl) GetAgentCredential() ([]byte, error) {
	certBytes, err := ioutil.ReadFile(ci.certFile)
	if err != nil {
		return nil, fmt.Errorf("Failed to read cert file: %s", ci.certFile)
	}
	return certBytes, nil
}

// GetCredentialType returns "onprem".
func (ci *OnPremClientImpl) GetCredentialType() string {
	return "onprem"
}

// getTLSCredentials creates transport credentials that are common to
// node agent and CA.
func getTLSCredentials(certificateFile string, keyFile string,
	caCertFile string) (credentials.TransportCredentials, error) {

	// Load the certificate from disk
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("Cannot load key pair: %s", err)
	}

	// Create a certificate pool
	certPool := x509.NewCertPool()
	bs, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("Failed to read CA cert: %s", err)
	}

	ok := certPool.AppendCertsFromPEM(bs)
	if !ok {
		return nil, fmt.Errorf("Failed to append certificates")
	}

	config := tls.Config{
		Certificates: []tls.Certificate{certificate},
	}
	config.RootCAs = certPool

	return credentials.NewTLS(&config), nil
}
