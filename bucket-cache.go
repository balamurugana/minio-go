/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2015-2017 MinIO, Inc.
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

package minio

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path"

	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/minio/minio-go/v7/pkg/signer"
)

// GetBucketLocation - get location for the bucket name from location cache, if not
// fetch freshly by making a new request.
func (c *Client) GetBucketLocation(ctx context.Context, bucketName string) (string, error) {
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return "", err
	}
	return c.getBucketLocation(ctx, bucketName)
}

// getBucketLocation - Get location for the bucketName from location map cache, if not
// fetch freshly by making a new request.
func (c *Client) getBucketLocation(ctx context.Context, bucketName string) (string, error) {
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return "", err
	}

	// Region set then no need to fetch bucket location.
	if c.region != "" {
		return c.region, nil
	}

	if location, ok := c.bucketLocCache.Get(bucketName); ok {
		return location, nil
	}

	// Initialize a new request.
	req, err := c.getBucketLocationRequest(ctx, bucketName)
	if err != nil {
		return "", err
	}

	// Initiate the request.
	resp, err := c.do(req)
	defer closeResponse(resp)
	if err != nil {
		return "", err
	}
	location, err := processBucketLocationResponse(resp, bucketName)
	if err != nil {
		return "", err
	}
	c.bucketLocCache.Set(bucketName, location)
	return location, nil
}

// processes the getBucketLocation http response from the server.
func processBucketLocationResponse(resp *http.Response, bucketName string) (bucketLocation string, err error) {
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			err = httpRespToErrorResponse(resp, bucketName, "")
			errResp := ToErrorResponse(err)
			// For access denied error, it could be an anonymous
			// request. Move forward and let the top level callers
			// succeed if possible based on their policy.
			switch errResp.Code {
			case NotImplemented:
				switch errResp.Server {
				case "AmazonSnowball":
					return "snowball", nil
				case "cloudflare":
					return "us-east-1", nil
				}
			case AuthorizationHeaderMalformed:
				fallthrough
			case InvalidRegion:
				fallthrough
			case AccessDenied:
				if errResp.Region == "" {
					return "us-east-1", nil
				}
				return errResp.Region, nil
			}
			return "", err
		}
	}

	// Extract location.
	var locationConstraint string
	err = xmlDecoder(resp.Body, &locationConstraint)
	if err != nil {
		return "", err
	}

	location := locationConstraint
	// Location is empty will be 'us-east-1'.
	if location == "" {
		location = "us-east-1"
	}

	// Location can be 'EU' convert it to meaningful 'eu-west-1'.
	if location == "EU" {
		location = "eu-west-1"
	}

	// Save the location into cache.

	// Return.
	return location, nil
}

// getBucketLocationRequest - Wrapper creates a new getBucketLocation request.
func (c *Client) getBucketLocationRequest(ctx context.Context, bucketName string) (*http.Request, error) {
	// Set location query.
	urlValues := make(url.Values)
	urlValues.Set("location", "")

	// Set get bucket location always as path style.
	targetURL := *c.endpointURL

	// as it works in makeTargetURL method from api.go file
	if h, p, err := net.SplitHostPort(targetURL.Host); err == nil {
		if targetURL.Scheme == "http" && p == "80" || targetURL.Scheme == "https" && p == "443" {
			targetURL.Host = h
			if ip := net.ParseIP(h); ip != nil && ip.To16() != nil {
				targetURL.Host = "[" + h + "]"
			}
		}
	}

	isVirtualStyle := c.isVirtualHostStyleRequest(targetURL, bucketName)

	var urlStr string

	if isVirtualStyle {
		urlStr = c.endpointURL.Scheme + "://" + bucketName + "." + targetURL.Host + "/?location"
	} else {
		targetURL.Path = path.Join(bucketName, "") + "/"
		targetURL.RawQuery = urlValues.Encode()
		urlStr = targetURL.String()
	}

	// Get a new HTTP request for the method.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	// Set UserAgent for the request.
	c.setUserAgent(req)

	// Get credentials from the configured credentials provider.
	value, err := c.credsProvider.GetWithContext(c.CredContext())
	if err != nil {
		return nil, err
	}

	var (
		signerType      = value.SignerType
		accessKeyID     = value.AccessKeyID
		secretAccessKey = value.SecretAccessKey
		sessionToken    = value.SessionToken
	)

	// Custom signer set then override the behavior.
	if c.overrideSignerType != credentials.SignatureDefault {
		signerType = c.overrideSignerType
	}

	// If signerType returned by credentials helper is anonymous,
	// then do not sign regardless of signerType override.
	if value.SignerType == credentials.SignatureAnonymous {
		signerType = credentials.SignatureAnonymous
	}

	if signerType.IsAnonymous() {
		return req, nil
	}

	if signerType.IsV2() {
		req = signer.SignV2(*req, accessKeyID, secretAccessKey, isVirtualStyle)
		return req, nil
	}

	// Set sha256 sum for signature calculation only with signature version '4'.
	contentSha256 := emptySHA256Hex
	if c.secure {
		contentSha256 = unsignedPayload
	}

	req.Header.Set("X-Amz-Content-Sha256", contentSha256)
	req = signer.SignV4(*req, accessKeyID, secretAccessKey, sessionToken, "us-east-1")
	return req, nil
}
