/// Entries copied from the Windows 11 24H2 Media Creation Tool catalog (products.xml in Products-Win11-24H2-6B.cab),
/// trimmed to a few files and one language block.
let productsXMLFixture = """
    <MCT>
      <Catalogs>
        <Catalog version="2.0">
          <PublishedMedia id="" release="">
            <Files>
              <File id="">
                <FileName>26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_x64FRE_zh-tw.esd</FileName>
                <LanguageCode>zh-tw</LanguageCode>
                <Language>Chinese (Traditional)</Language>
                <Edition>Professional</Edition>
                <Architecture>x64</Architecture>
                <Size>4689342463</Size>
                <Sha1>0000000000000000000000000000000000000001</Sha1>
                <FilePath>http://dl.delivery.mp.microsoft.com/filestreamingservice/files/00000000-0000-0000-0000-000000000001/26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_x64FRE_zh-tw.esd</FilePath>
                <Key />
                <Architecture_Loc>%ARCH_64%</Architecture_Loc>
                <Edition_Loc>%CLIENT%</Edition_Loc>
                <IsRetailOnly>False</IsRetailOnly>
              </File>
              <File id="">
                <FileName>26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd</FileName>
                <LanguageCode>zh-tw</LanguageCode>
                <Language>Chinese (Traditional)</Language>
                <Edition>Professional</Edition>
                <Architecture>ARM64</Architecture>
                <Size>4670731279</Size>
                <Sha1>d2bd29d2e839ada7c25dbb8befb17dcabc645adb</Sha1>
                <FilePath>http://dl.delivery.mp.microsoft.com/filestreamingservice/files/5c6e1f2f-087e-45c4-a3c6-59745c776a74/26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd</FilePath>
                <Key />
                <Architecture_Loc>%ARCH_64%</Architecture_Loc>
                <Edition_Loc>%CLIENT%</Edition_Loc>
                <IsRetailOnly>False</IsRetailOnly>
              </File>
              <File id="">
                <FileName>26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd</FileName>
                <LanguageCode>zh-tw</LanguageCode>
                <Language>Chinese (Traditional)</Language>
                <Edition>Core</Edition>
                <Architecture>ARM64</Architecture>
                <Size>4670731279</Size>
                <Sha1>d2bd29d2e839ada7c25dbb8befb17dcabc645adb</Sha1>
                <FilePath>http://dl.delivery.mp.microsoft.com/filestreamingservice/files/5c6e1f2f-087e-45c4-a3c6-59745c776a74/26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd</FilePath>
                <Key />
                <Architecture_Loc>%ARCH_64%</Architecture_Loc>
                <Edition_Loc>%BASE%</Edition_Loc>
                <IsRetailOnly>False</IsRetailOnly>
              </File>
              <File id="">
                <FileName>26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_en-us.esd</FileName>
                <LanguageCode>en-us</LanguageCode>
                <Language>English (United States)</Language>
                <Edition>Professional</Edition>
                <Architecture>ARM64</Architecture>
                <Size>4623104862</Size>
                <Sha1>0000000000000000000000000000000000000002</Sha1>
                <FilePath>http://dl.delivery.mp.microsoft.com/filestreamingservice/files/00000000-0000-0000-0000-000000000002/26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_en-us.esd</FilePath>
                <Key />
                <Architecture_Loc>%ARCH_64%</Architecture_Loc>
                <Edition_Loc>%CLIENT%</Edition_Loc>
                <IsRetailOnly>False</IsRetailOnly>
              </File>
            </Files>
            <Languages>
              <Language LanguageCode="default">
                <PRO>Windows 11 Pro</PRO>
                <ARCH_64>64-bit (x64)</ARCH_64>
              </Language>
            </Languages>
          </PublishedMedia>
        </Catalog>
      </Catalogs>
    </MCT>
    """
