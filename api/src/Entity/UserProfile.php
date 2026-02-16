<?php

namespace App\Entity;

use App\Entity\Enum\Gender;
use App\Repository\UserProfileRepository;
use Doctrine\DBAL\Types\Types;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: UserProfileRepository::class)]
#[ORM\HasLifecycleCallbacks]
class UserProfile
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\OneToOne(inversedBy: 'profile')]
    #[ORM\JoinColumn(nullable: false)]
    private User $user;

    #[ORM\Column(length: 35, nullable: true)]
    private ?string $alias = null;

    #[ORM\Column(type: Types::STRING, length: 10, enumType: Gender::class)]
    private Gender $gender = Gender::NONE;

    #[ORM\Column(type: Types::TEXT, nullable: true)]
    private ?string $entryMessage = null;

    #[ORM\Column(type: Types::TEXT, nullable: true)]
    private ?string $exitMessage = null;

    #[ORM\Column(type: Types::TEXT, nullable: true)]
    private ?string $bio = null;

    #[ORM\Column(length: 255, nullable: true)]
    private ?string $avatarUrl = null;

    #[ORM\Column(length: 1, nullable: true)]
    private ?string $preferredColor = null;

    #[ORM\Column(length: 50, nullable: true)]
    private ?string $timezone = null;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    #[ORM\Column]
    private \DateTimeImmutable $updatedAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
        $this->updatedAt = new \DateTimeImmutable();
    }

    #[ORM\PreUpdate]
    public function onPreUpdate(): void
    {
        $this->updatedAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function setUser(User $user): static
    {
        $this->user = $user;
        return $this;
    }

    public function getAlias(): ?string
    {
        return $this->alias;
    }

    public function setAlias(?string $alias): static
    {
        $this->alias = $alias;
        return $this;
    }

    public function getDisplayName(): string
    {
        return $this->alias ?? $this->user->getUsername();
    }

    public function getGender(): Gender
    {
        return $this->gender;
    }

    public function setGender(Gender $gender): static
    {
        $this->gender = $gender;
        return $this;
    }

    public function getEntryMessage(): ?string
    {
        return $this->entryMessage;
    }

    public function setEntryMessage(?string $entryMessage): static
    {
        $this->entryMessage = $entryMessage;
        return $this;
    }

    public function getExitMessage(): ?string
    {
        return $this->exitMessage;
    }

    public function setExitMessage(?string $exitMessage): static
    {
        $this->exitMessage = $exitMessage;
        return $this;
    }

    public function getBio(): ?string
    {
        return $this->bio;
    }

    public function setBio(?string $bio): static
    {
        $this->bio = $bio;
        return $this;
    }

    public function getAvatarUrl(): ?string
    {
        return $this->avatarUrl;
    }

    public function setAvatarUrl(?string $avatarUrl): static
    {
        $this->avatarUrl = $avatarUrl;
        return $this;
    }

    public function getPreferredColor(): ?string
    {
        return $this->preferredColor;
    }

    public function setPreferredColor(?string $preferredColor): static
    {
        $this->preferredColor = $preferredColor;
        return $this;
    }

    public function getTimezone(): ?string
    {
        return $this->timezone;
    }

    public function setTimezone(?string $timezone): static
    {
        $this->timezone = $timezone;
        return $this;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }

    public function getUpdatedAt(): \DateTimeImmutable
    {
        return $this->updatedAt;
    }
}
