<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Repository\ActionRepository;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: ActionRepository::class)]
#[ORM\Table(name: 'zchat_action')]
#[ORM\UniqueConstraint(name: 'zchat_unique_action_slug', columns: ['slug'])]
#[ORM\Index(columns: ['action_list_slug', 'is_active'], name: 'idx_zchat_action_list')]
class Action
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\Column(length: 30)]
    private string $slug;

    #[ORM\Column(length: 50)]
    private string $actionListSlug = 'default';

    #[ORM\Column(length: 255)]
    private string $templateSelf;

    #[ORM\Column(length: 255)]
    private string $templateTarget;

    #[ORM\Column(length: 255)]
    private string $templateNoTarget;

    #[ORM\Column]
    private bool $isActive = true;

    #[ORM\Column]
    private int $sortOrder = 0;

    public function __construct()
    {
        $this->id = Uuid::v7();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getSlug(): string
    {
        return $this->slug;
    }

    public function setSlug(string $slug): static
    {
        $this->slug = $slug;
        return $this;
    }

    public function getActionListSlug(): string
    {
        return $this->actionListSlug;
    }

    public function setActionListSlug(string $actionListSlug): static
    {
        $this->actionListSlug = $actionListSlug;
        return $this;
    }

    public function getTemplateSelf(): string
    {
        return $this->templateSelf;
    }

    public function setTemplateSelf(string $templateSelf): static
    {
        $this->templateSelf = $templateSelf;
        return $this;
    }

    public function getTemplateTarget(): string
    {
        return $this->templateTarget;
    }

    public function setTemplateTarget(string $templateTarget): static
    {
        $this->templateTarget = $templateTarget;
        return $this;
    }

    public function getTemplateNoTarget(): string
    {
        return $this->templateNoTarget;
    }

    public function setTemplateNoTarget(string $templateNoTarget): static
    {
        $this->templateNoTarget = $templateNoTarget;
        return $this;
    }

    public function isActive(): bool
    {
        return $this->isActive;
    }

    public function setIsActive(bool $isActive): static
    {
        $this->isActive = $isActive;
        return $this;
    }

    public function getSortOrder(): int
    {
        return $this->sortOrder;
    }

    public function setSortOrder(int $sortOrder): static
    {
        $this->sortOrder = $sortOrder;
        return $this;
    }

    public function format(?string $actor, ?string $target = null): string
    {
        if ($actor === $target) {
            return str_replace('{actor}', $actor ?? 'Someone', $this->templateSelf);
        }

        if ($target !== null) {
            return str_replace(
                ['{actor}', '{target}'],
                [$actor ?? 'Someone', $target],
                $this->templateTarget
            );
        }

        return str_replace('{actor}', $actor ?? 'Someone', $this->templateNoTarget);
    }
}
