// Copyright 2014 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package ca

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/policy"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/auth"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/config"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/signer"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/signer/remote"
)

// CertificateAuthorityImpl represents a CA that signs certificates, CRLs, and
// OCSP responses.
type CertificateAuthorityImpl struct {
	profile string
	Signer  signer.Signer
	SA      core.StorageAuthority
	PA      core.PolicyAuthority
	DB      core.CertificateAuthorityDatabase
	log     *blog.AuditLogger
	Prefix  int // Prepended to the serial number
}

// NewCertificateAuthorityImpl creates a CA that talks to a remote CFSSL
// instance.  (To use a local signer, simply instantiate CertificateAuthorityImpl
// directly.)  Communications with the CA are authenticated with MACs,
// using CFSSL's authenticated signature scheme.  A CA created in this way
// issues for a single profile on the remote signer, which is indicated
// by name in this constructor.
func NewCertificateAuthorityImpl(logger *blog.AuditLogger, hostport string, authKey string, profile string, serialPrefix int, cadb core.CertificateAuthorityDatabase) (ca *CertificateAuthorityImpl, err error) {
	logger.Notice("Certificate Authority Starting")

	// Create the remote signer
	localProfile := config.SigningProfile{
		Expiry:       time.Hour, // BOGUS: Required by CFSSL, but not used
		RemoteName:   hostport,  // BOGUS: Only used as a flag by CFSSL
		RemoteServer: hostport,
		UseSerialSeq: true,
	}

	localProfile.Provider, err = auth.New(authKey, nil)
	if err != nil {
		return
	}

	signer, err := remote.NewSigner(&config.Signing{Default: &localProfile})
	if err != nil {
		return
	}

	pa := policy.NewPolicyAuthorityImpl(logger)

	ca = &CertificateAuthorityImpl{
		Signer:  signer,
		profile: profile,
		PA:      pa,
		DB:      cadb,
		Prefix:  serialPrefix,
		log:     logger,
	}
	return
}

// IssueCertificate attempts to convert a CSR into a signed Certificate, while
// enforcing all policies.
func (ca *CertificateAuthorityImpl) IssueCertificate(csr x509.CertificateRequest) (cert core.Certificate, err error) {
	// XXX Take in authorizations and verify that union covers CSR?
	// Pull hostnames from CSR
	hostNames := csr.DNSNames // DNSNames + CN from CSR
	var commonName string
	if len(csr.Subject.CommonName) > 0 {
		commonName = csr.Subject.CommonName
	} else if len(hostNames) > 0 {
		commonName = hostNames[0]
	} else {
		err = errors.New("Cannot issue a certificate without a hostname.")
		ca.log.WarningErr(err)
		return
	}

	if len(hostNames) == 0 {
		hostNames = []string{commonName}
	}

	identifier := core.AcmeIdentifier{Type: core.IdentifierDNS, Value: commonName}
	if err = ca.PA.WillingToIssue(identifier); err != nil {
		err = errors.New("Policy forbids issuing for name " + commonName)
		ca.log.AuditErr(err)
		return
	}
	for _, name := range hostNames {
		identifier = core.AcmeIdentifier{Type: core.IdentifierDNS, Value: name}
		if err = ca.PA.WillingToIssue(identifier); err != nil {
			err = errors.New("Policy forbids issuing for name " + name)
			ca.log.AuditErr(err)
			return
		}
	}

	// Convert the CSR to PEM
	csrPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csr.Raw,
	}))

	// Get the next serial number
	ca.DB.Begin()
	serialDec, err := ca.DB.IncrementAndGetSerial()
	if err != nil {
		return
	}
	serialHex := fmt.Sprintf("%02X%014X", ca.Prefix, serialDec)

	// Send the cert off for signing
	req := signer.SignRequest{
		Request: csrPEM,
		Profile: ca.profile,
		Hosts:   hostNames,
		Subject: &signer.Subject{
			CN: commonName,
		},
		SerialSeq: serialHex,
	}

	certPEM, err := ca.Signer.Sign(req)
	if err != nil {
		ca.DB.Rollback()
		return
	}

	if len(certPEM) == 0 {
		err = errors.New("No certificate returned by server")
		ca.log.WarningErr(err)
		ca.DB.Rollback()
		return
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		err = errors.New("Invalid certificate value returned")
		ca.log.WarningErr(err)
		ca.DB.Rollback()
		return
	}
	certDER := block.Bytes

	cert = core.Certificate{
		DER:    certDER,
		Status: core.StatusValid,
	}
	if err != nil {
		return core.Certificate{}, err
	}

	// Store the cert with the certificate authority, if provided
	_, err = ca.SA.AddCertificate(certDER)
	if err != nil {
		ca.DB.Rollback()
		return
	}

	ca.DB.Commit()
	return
}
