using Myriagent.Agents;
using Myriagent.Tests.TestSupport;
using Newtonsoft.Json.Linq;

namespace Myriagent.Tests.Hub;

/// <summary>
/// 中樞的「使用現有的設定資料夾」（accountAdd 帶 directory）：把使用者原有的資料夾加成帳戶、網頁看到的欄位、不能共用預設帳戶的設定、
/// 檢查不過只提示、移除時不刪資料夾
/// </summary>
[Collection(AppPathsCollection.Name)]
public sealed class AgentHubExistingFolderTests
{
    [Fact]
    public void AccountAdd_帶資料夾_把現有的設定資料夾加成帳戶_不共用_失敗只提示_移除不刪()
    {
        using var h = new HubHarness();
        using var temp = new TempDirectory("hub-existing");
        var folder = temp.CreateDirectory(".claude-work");
        var marker = temp.WriteFile(Path.Combine(".claude-work", ".claude.json"), "{}");

        h.Command(h.Main, "accountAdd", new { provider = "Claude", label = "", color = "#19C3A6", directory = folder });

        var added = h.OnUi(() => h.Accounts.Accounts.Single(a => a.ExistingFolder));
        added.Provider.Should().Be(AgentProvider.Claude);
        added.ConfigDirectory.Should().Be(folder);
        added.Label.Should().Be("Claude work");
        added.ShareCustomizations.Should().BeFalse();
        h.Main.Last("toast")!.Value<string>("text").Should().Be($"已加入「Claude work」，使用 {folder}");
        h.Main.Last("toast")!.Value<string>("level").Should().Be("success");

        h.Command(h.Main, "ready");
        var init = h.Main.Last("init")!;
        var views = (JArray)init["accounts"]!;
        var view = views.Single(a => (string?)a["id"] == added.Id);
        view["existingFolder"]!.Value<bool>().Should().BeTrue();
        view["isSystemDefault"]!.Value<bool>().Should().BeFalse();
        view["shareCustomizations"]!.Value<bool>().Should().BeFalse();
        view["configDirectory"]!.Value<string>().Should().Be(folder);
        views.Single(a => (string?)a["id"] == h.Claude2.Id)["existingFolder"]!.Value<bool>().Should().BeFalse("app 建的隔離帳戶不是");
        views.Single(a => (string?)a["id"] == h.Claude.Id)["existingFolder"]!.Value<bool>().Should().BeFalse("本機預設帳戶不是");
        var providers = (JArray)init["providers"]!;
        providers.Should().OnlyContain(p => p["existingFolders"] is JArray);
        ((JArray)providers.Single(p => (string?)p["key"] == "Antigravity")["existingFolders"]!).Should().BeEmpty("沒有隔離帳戶");

        h.Command(h.Main, "accountUpdate", new { accountId = added.Id, shareCustomizations = true });
        h.OnUi(() => h.Accounts.Find(added.Id)!.ShareCustomizations).Should().BeFalse("現有的資料夾不能打開共用");

        var count = h.OnUi(() => h.Accounts.Accounts.Count);
        h.Command(h.Main, "accountAdd", new { provider = "Claude", label = "再一次", directory = folder });
        h.Main.Last("toast")!.Value<string>("text").Should().Be($"已經有帳戶在用這個資料夾：{folder}");
        h.Main.Last("toast")!.Value<string>("level").Should().Be("error");
        h.Command(h.Main, "accountAdd", new { provider = "Claude", directory = temp.Combine("沒有") });
        h.Main.Last("toast")!.Value<string>("text").Should().StartWith("找不到這個資料夾");
        h.Command(h.Main, "accountAdd", new { provider = "Antigravity", directory = folder });
        h.Main.Last("toast")!.Value<string>("text").Should().Contain("一台電腦只有一個登入");
        h.OnUi(() => h.Accounts.Accounts.Count).Should().Be(count, "檢查不過不會加入，也不會落回一般的新增");

        h.Command(h.Main, "accountRemove", new { accountId = added.Id, deleteData = true });
        h.OnUi(() => h.Accounts.Find(added.Id)).Should().BeNull();
        File.Exists(marker).Should().BeTrue("使用者原有的資料夾絕不刪，勾了刪除資料也一樣");
    }

    [Fact]
    public void AccountAdd_directory是空白或不是字串_照一般的新增()
    {
        using var h = new HubHarness();
        var count = h.OnUi(() => h.Accounts.Accounts.Count);

        h.Command(h.Main, "accountAdd", new { provider = "Codex", label = "一般新增", directory = "   " });

        h.OnUi(() => h.Accounts.Accounts.Count).Should().Be(count + 1);
        var added = h.OnUi(() => h.Accounts.Accounts.Single(a => a.Label == "一般新增"));
        added.ExistingFolder.Should().BeFalse();
        added.ConfigDirectory.Should().StartWith(Myriagent.Core.AppPaths.AccountsDirectory, "app 自己建的隔離資料夾");
        h.OnUi(() => h.Accounts.Remove(added.Id, deleteData: true));
    }
}
