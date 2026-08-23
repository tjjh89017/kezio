/*
Copyright 2026.

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

package v1alpha2

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

var _ = Describe("Site Webhook", func() {
	var (
		obj       *keziov1alpha2.Site
		oldObj    *keziov1alpha2.Site
		validator SiteCustomValidator
		subnetSeq int
	)

	BeforeEach(func() {
		obj = &keziov1alpha2.Site{}
		oldObj = &keziov1alpha2.Site{}
		validator = SiteCustomValidator{Client: k8sClient}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	// ensureSite creates a placeholder Site named name, ignoring
	// AlreadyExists: the Subnet webhook now requires spec.siteRef to
	// resolve, and this suite's Site fixtures under test are never
	// themselves persisted (only passed to the validator directly), so a
	// real backing object is created here purely to satisfy that check.
	ensureSite := func(name string) {
		site := &keziov1alpha2.Site{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}
		err := k8sClient.Create(ctx, site)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	// newSubnet creates a Subnet whose spec.siteRef names siteRefName, so
	// tests can control whether it points back at the Site under test.
	newSubnet := func(siteRefName string) *keziov1alpha2.Subnet {
		subnetSeq++
		ensureSite(siteRefName)
		subnet := &keziov1alpha2.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("site-webhook-subnet-%d", subnetSeq),
				Namespace: "default",
			},
			Spec: keziov1alpha2.SubnetSpec{
				SiteRef:         keziov1alpha2.NameRef{Name: siteRefName},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: &keziov1alpha2.NameRef{Name: "bootd-net"},
				DHCP: &keziov1alpha2.SubnetDHCP{
					Mode: keziov1alpha2.SubnetDHCPModeProxy,
				},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		return subnet
	}

	Context("When creating or updating Site under Validating Webhook", func() {
		It("admits a non-seeding Site with no tracker", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-no-seeding-no-tracker", Namespace: "default"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies a non-seeding Site carrying a tracker", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-no-seeding-with-tracker", Namespace: "default"}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("denies a Site whose seederSubnetRef names a Subnet that does not exist", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-missing-subnet", Namespace: "default"}
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: "no-such-subnet"}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("denies a Site whose seederSubnetRef names a Subnet belonging to another Site", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-foreign-subnet", Namespace: "default"}
			subnet := newSubnet("some-other-site")
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			err := func() error { _, err := validator.ValidateCreate(ctx, obj); return err }()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(subnet.Name))
		})

		It("denies a seeding Site with no tracker at all", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-seeding-no-tracker", Namespace: "default"}
			subnet := newSubnet("site-seeding-no-tracker")
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("admits a valid seeding Site with tracker.ip", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-seeding-tracker-ip", Namespace: "default"}
			subnet := newSubnet("site-seeding-tracker-ip")
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("admits a valid seeding Site with tracker.externalURL", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-seeding-tracker-url", Namespace: "default"}
			subnet := newSubnet("site-seeding-tracker-url")
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{ExternalURL: "http://tracker.example.com:6969/announce"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies a Site whose seederSubnetRef names a Subnet whose siteRef names a same-named Site in another namespace", func() {
			// The Subnet's siteRef names "hq" resolved in the Subnet's own
			// namespace ("other-ns"), not the "hq" under test here (in
			// "default"). Comparing names alone would wrongly treat these
			// as the same Site.
			otherNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-webhook-other-ns"}}
			Expect(k8sClient.Create(ctx, otherNS)).To(Or(Succeed(), WithTransform(apierrors.IsAlreadyExists, BeTrue())))

			foreignSiteName := "hq"
			foreignSite := &keziov1alpha2.Site{
				ObjectMeta: metav1.ObjectMeta{Name: foreignSiteName, Namespace: otherNS.Name},
			}
			Expect(k8sClient.Create(ctx, foreignSite)).To(Or(Succeed(), WithTransform(apierrors.IsAlreadyExists, BeTrue())))

			subnet := &keziov1alpha2.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "site-webhook-cross-ns-subnet", Namespace: otherNS.Name},
				Spec: keziov1alpha2.SubnetSpec{
					SiteRef:         keziov1alpha2.NameRef{Name: foreignSiteName},
					CIDR:            "192.0.2.0/24",
					BootdServerIP:   "192.0.2.2",
					BootdNetworkRef: &keziov1alpha2.NameRef{Name: "bootd-net"},
					DHCP: &keziov1alpha2.SubnetDHCP{
						Mode: keziov1alpha2.SubnetDHCPModeProxy,
					},
				},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

			obj.ObjectMeta = metav1.ObjectMeta{Name: foreignSiteName, Namespace: "default"}
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name, Namespace: otherNS.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(subnet.Name))
		})

		It("admits a Site whose seederSubnetRef correctly names a Subnet in the same namespace", func() {
			obj.ObjectMeta = metav1.ObjectMeta{Name: "site-correct-back-ref", Namespace: "default"}
			subnet := newSubnet("site-correct-back-ref")
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		DescribeTable("tracker.externalURL validation",
			func(externalURL string, shouldSucceed bool) {
				obj.ObjectMeta = metav1.ObjectMeta{Name: "site-external-url-test", Namespace: "default"}
				subnet := newSubnet(obj.Name)
				obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
				obj.Spec.Tracker = keziov1alpha2.SiteTracker{ExternalURL: externalURL}
				_, err := validator.ValidateCreate(ctx, obj)
				if shouldSucceed {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(HaveOccurred())
				}
			},
			Entry("no scheme", "tracker.example.com/announce", false),
			Entry("unsupported scheme", "ftp://tracker.example.com/announce", false),
			Entry("no host", "http:///announce", false),
			Entry("valid http", "http://tracker.example.com:6969/announce", true),
			Entry("valid https", "https://tracker.example.com/announce", true),
			Entry("valid udp", "udp://tracker.example.com:6969", true),
		)

		It("applies the same rules on update", func() {
			oldObj.ObjectMeta = metav1.ObjectMeta{Name: "site-update-check", Namespace: "default"}
			subnet := newSubnet("some-other-site")
			obj.ObjectMeta = oldObj.ObjectMeta
			obj.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnet.Name}
			obj.Spec.Tracker = keziov1alpha2.SiteTracker{IP: "192.0.2.9"}
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().To(HaveOccurred())
		})
	})
})
